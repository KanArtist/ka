import { Injectable, computed, inject, signal } from '@angular/core';
import { setActors } from './ui/actor';
import { Toasts } from './ui/toast';
import { Api, ActorOut, BoardOut, InboxOut, KaEvent, SchemaOut, SearchOut, Session, StoreOut, Summary, WorkspaceOut } from './api/api';

const EVENT_TYPES = [
  'ticket.created',
  'ticket.updated',
  'ticket.moved',
  'ticket.removed',
  'comment.created',
  'claim.changed',
  'question.opened',
  'question.answered',
  'sync.state',
  'sync.conflict_preserved',
  'store.changed',
  'store.compacted',
  'presence.changed',
  'reset',
];

/** How many recent events the Activity view keeps in memory. */
const EVENT_LIMIT = 2000;
/** Reconnect backoff after the stream closes or a resync fails: 1 s, doubling, capped. */
const RETRY_MIN_MS = 1000;
const RETRY_MAX_MS = 30_000;

export type Connection = 'connecting' | 'live' | 'reconnecting';

/**
 * The workspace as the browser sees it: a snapshot from REST, kept current
 * by the SSE stream. Snapshot first, then subscribe from the snapshot's
 * Ka-Event-Seq, so nothing is missed between the two (docs/api.md).
 */
@Injectable({ providedIn: 'root' })
export class Live {
  private api = inject(Api);
  private toasts = inject(Toasts);
  private source?: EventSource;
  private lastSeq = 0;
  /** The newest event in `events`, so a replay after a resync does not list it twice. */
  private newestEvent = 0;
  /** Bumped by each snapshot: an older one still in flight is dropped. */
  private generation = 0;
  private retryMs = RETRY_MIN_MS;
  private retryTimer?: ReturnType<typeof setTimeout>;

  readonly workspace = signal<WorkspaceOut | null>(null);
  readonly schema = signal<SchemaOut | null>(null);
  readonly stores = signal<StoreOut[]>([]);
  readonly tickets = signal<ReadonlyMap<string, Summary>>(new Map());
  readonly events = signal<KaEvent[]>([]);
  readonly presence = signal<Session[]>([]);
  readonly inbox = signal<InboxOut | null>(null);
  /** Saved boards from every store (boards/*.yml), namespaced store/key. */
  readonly boards = signal<BoardOut[]>([]);
  readonly connection = signal<Connection>('connecting');
  readonly loaded = signal(false);
  /** Bumped on every ticket change, for views that watch one ticket. */
  readonly ticketTick = signal(0);
  /** Bumped when a question is opened or answered (is:question matches change without a ticket change). */
  readonly questionTick = signal(0);
  /**
   * Bumped when a store's commit history may have been rewritten: a
   * store.compacted event (compacted here or adopted from upstream), or a
   * store.changed, which is all the daemon reports when another process on
   * the same clone (ka compact from a terminal) compacts it. Every SHA from
   * before is then gone, so views that show history (activity, timelines,
   * summaries) refetch it.
   */
  readonly historyEpoch = signal(0);
  /** The ID of the last ticket an event touched, with the actor and event ID, for highlights. */
  readonly touched = signal<{ id: string; actor?: string; at: number; seq: number } | null>(null);

  readonly you = computed(() => this.workspace()?.you ?? '');
  /** project -> status -> category (unstarted, started, completed, canceled, backlog, triage). */
  private readonly categories = computed(() => {
    const m = new Map<string, Map<string, string>>();
    for (const p of this.schema()?.projects ?? []) {
      m.set(p.key, new Map((p.statuses ?? []).map((s) => [s.name, s.category])));
    }
    return m;
  });

  category(t: Pick<Summary, 'project' | 'status'>): string {
    return this.categories().get(t.project)?.get(t.status) ?? 'unstarted';
  }

  isClosed(t: Pick<Summary, 'project' | 'status'>): boolean {
    const c = this.category(t);
    return c === 'completed' || c === 'canceled';
  }
  readonly inboxCount = computed(() => this.inbox()?.questions?.length ?? 0);
  readonly syncTrouble = computed(() =>
    this.stores().filter((s) => s.state !== 'synced' && s.state !== 'local'),
  );

  async start(): Promise<void> {
    await this.snapshot();
    this.subscribe();
  }

  /** Loads the snapshot; false if a newer snapshot started meanwhile. */
  private async snapshot(): Promise<boolean> {
    const gen = ++this.generation;
    void this.refreshActors();
    const [ws, schema, stores, tickets, presence, inbox, boards] = await Promise.all([
      this.api.get<WorkspaceOut>('/workspace'),
      this.api.get<SchemaOut>('/schema'),
      this.api.get<{ stores: StoreOut[] }>('/stores'),
      this.api.get<SearchOut>('/tickets?all=true&limit=-1&include=fields'),
      this.api.get<{ sessions: Session[] }>('/presence'),
      this.api.get<InboxOut>('/inbox?since=7d'),
      this.api.get<{ boards: BoardOut[] }>('/boards'),
    ]);
    if (gen !== this.generation) return false;
    this.workspace.set(ws.body);
    this.schema.set(schema.body);
    this.stores.set(stores.body.stores ?? []);
    this.tickets.set(new Map((tickets.body.tickets ?? []).map((t) => [t.id, t])));
    this.presence.set(presence.body.sessions ?? []);
    this.inbox.set(inbox.body);
    this.boards.set(boards.body.boards ?? []);
    // Resume from the oldest of the snapshots; replayed events are idempotent.
    this.lastSeq = Math.min(ws.seq, stores.seq, tickets.seq, presence.seq, inbox.seq);
    this.ticketTick.update((n) => n + 1);
    this.loaded.set(true);
    return true;
  }

  private subscribe(): void {
    this.source?.close();
    const es = new EventSource(`/api/v1/events?last_event_id=${this.lastSeq}`);
    this.source = es;
    es.onopen = () => {
      this.connection.set('live');
      this.retryMs = RETRY_MIN_MS;
    };
    es.onerror = () => {
      this.connection.set('reconnecting');
      // EventSource retries network errors itself, but any non-200 answer
      // (a restarted daemon that 401s the old cookie, a 502) closes it for good.
      if (es.readyState === EventSource.CLOSED && es === this.source) this.retry();
    };
    for (const type of EVENT_TYPES) {
      es.addEventListener(type, (m) => {
        if (es === this.source) this.apply(JSON.parse((m as MessageEvent).data) as KaEvent);
      });
    }
  }

  /**
   * A fresh snapshot, then a fresh stream from its Ka-Event-Seq: events that
   * arrived while it loaded are replayed over it, so the older snapshot never
   * wins. On failure it retries with backoff; a 401 shows the sign-in gate.
   */
  private async resync(): Promise<void> {
    clearTimeout(this.retryTimer);
    this.retryTimer = undefined;
    try {
      if (await this.snapshot()) this.subscribe();
    } catch {
      if (this.api.unauthorized()) {
        this.source?.close();
        return;
      }
      this.connection.set('reconnecting');
      this.retry();
    }
  }

  private retry(): void {
    if (this.retryTimer) return;
    const ms = this.retryMs;
    this.retryMs = Math.min(this.retryMs * 2, RETRY_MAX_MS);
    this.retryTimer = setTimeout(() => void this.resync(), ms);
  }

  async refreshBoards(): Promise<void> {
    try {
      this.boards.set((await this.api.get<{ boards: BoardOut[] }>('/boards')).body.boards ?? []);
    } catch {
      /* keep the old list */
    }
  }

  /** The actor registries, for display names (best effort: handles show until it loads). */
  private async refreshActors(): Promise<void> {
    try {
      setActors((await this.api.get<{ actors: ActorOut[] | null }>('/actors')).body.actors ?? []);
    } catch {
      /* keep the old names */
    }
  }

  private refreshInbox = debounce(async () => {
    try {
      this.inbox.set((await this.api.get<InboxOut>('/inbox?since=7d')).body);
    } catch {
      /* keep the old one */
    }
  }, 150);

  private apply(e: KaEvent): void {
    this.lastSeq = Math.max(this.lastSeq, e.id);
    // Replayed after a resync: re-apply the state, but do not list or highlight it again.
    const fresh = e.id > this.newestEvent;
    this.newestEvent = Math.max(this.newestEvent, e.id);
    switch (e.type) {
      case 'store.changed':
        if (e.data?.what === 'boards') {
          void this.refreshBoards();
          break;
        }
        void this.resync();
        if (fresh) this.historyEpoch.update((n) => n + 1);
        return;
      case 'reset':
        void this.resync();
        return;
      case 'store.compacted':
        // The tree is unchanged, but an adopted history reloaded the index
        // and every SHA changed: start over, like a reset, and keep the
        // event for the Activity view. A replay of it was handled already.
        if (!fresh) break;
        void this.resync();
        this.historyEpoch.update((n) => n + 1);
        this.toasts.show(
          e.data?.adopted ? `Store ${e.store} adopted a compacted history` : `Store ${e.store}'s history was compacted`,
          'info',
          'Links to older commits no longer resolve; Sync & health has the summary of what came before.',
        );
        break;
      case 'ticket.created':
      case 'ticket.updated':
      case 'ticket.moved':
      case 'claim.changed':
        if (e.data?.ticket?.id) this.putTicket(e.data.ticket);
        break;
      case 'ticket.removed':
        if (e.ticket) {
          const m = new Map(this.tickets());
          m.delete(e.ticket);
          this.tickets.set(m);
          this.ticketTick.update((n) => n + 1);
        }
        break;
      case 'comment.created':
        this.ticketTick.update((n) => n + 1);
        break;
      case 'question.opened':
      case 'question.answered':
        this.questionTick.update((n) => n + 1);
        this.refreshInbox();
        break;
      case 'presence.changed':
        this.applyPresence(e.data?.state, e.data?.session);
        break;
      case 'sync.state':
        if (e.data?.store) {
          this.stores.update((ss) => ss.map((s) => (s.store === e.data.store ? { ...s, ...e.data } : s)));
        }
        break;
    }
    if (fresh && e.ticket) this.touched.set({ id: e.ticket, actor: e.actor, at: Date.now(), seq: e.id });
    if (fresh && (e.type !== 'presence.changed' || e.data?.state !== 'active')) {
      this.events.update((list) => [e, ...list].slice(0, EVENT_LIMIT));
    }
  }

  /**
   * Show a change before the daemon confirms it (a dragged card lands at
   * once). The event stream overwrites it with the truth; call the returned
   * function to undo it if the write fails.
   */
  patchTicket(id: string, patch: Partial<Summary>): () => void {
    const old = this.tickets().get(id);
    if (!old) return () => {};
    this.putTicket({ ...old, ...patch });
    return () => {
      const cur = this.tickets().get(id);
      // Only undo if nothing newer arrived meanwhile.
      if (cur && Object.entries(patch).every(([k, v]) => (cur as any)[k] === v)) this.putTicket(old);
    };
  }

  private putTicket(t: Summary): void {
    const m = new Map(this.tickets());
    m.set(t.id, t);
    this.tickets.set(m);
    this.ticketTick.update((n) => n + 1);
  }

  private applyPresence(state: string, s: Session | undefined): void {
    if (!s) return;
    this.presence.update((list) => {
      const rest = list.filter((x) => x.id !== s.id);
      return state === 'disconnected' ? rest : [s, ...rest];
    });
  }
}

function debounce(fn: () => void, ms: number): () => void {
  let t: ReturnType<typeof setTimeout> | undefined;
  return () => {
    clearTimeout(t);
    t = setTimeout(fn, ms);
  };
}
