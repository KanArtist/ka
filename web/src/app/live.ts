import { Injectable, computed, inject, signal } from '@angular/core';
import { Api, BoardOut, InboxOut, KaEvent, SchemaOut, SearchOut, Session, StoreOut, Summary, WorkspaceOut } from './api/api';

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
  'presence.changed',
  'reset',
];

/** How many recent events the Activity view keeps in memory. */
const EVENT_LIMIT = 2000;

export type Connection = 'connecting' | 'live' | 'reconnecting';

/**
 * The workspace as the browser sees it: a snapshot from REST, kept current
 * by the SSE stream. Snapshot first, then subscribe from the snapshot's
 * Ka-Event-Seq, so nothing is missed between the two (docs/api.md).
 */
@Injectable({ providedIn: 'root' })
export class Live {
  private api = inject(Api);
  private source?: EventSource;
  private lastSeq = 0;

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
  /** The ID of the last ticket an event touched, with the actor, for highlights. */
  readonly touched = signal<{ id: string; actor?: string; at: number } | null>(null);

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

  private async snapshot(): Promise<void> {
    const [ws, schema, stores, tickets, presence, inbox, boards] = await Promise.all([
      this.api.get<WorkspaceOut>('/workspace'),
      this.api.get<SchemaOut>('/schema'),
      this.api.get<{ stores: StoreOut[] }>('/stores'),
      this.api.get<SearchOut>('/tickets?all=true&limit=-1&include=fields'),
      this.api.get<{ sessions: Session[] }>('/presence'),
      this.api.get<InboxOut>('/inbox?since=7d'),
      this.api.get<{ boards: BoardOut[] }>('/boards'),
    ]);
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
  }

  private subscribe(): void {
    this.source?.close();
    const es = new EventSource(`/api/v1/events?last_event_id=${this.lastSeq}`);
    this.source = es;
    es.onopen = () => this.connection.set('live');
    es.onerror = () => this.connection.set(es.readyState === EventSource.CLOSED ? 'reconnecting' : 'reconnecting');
    for (const type of EVENT_TYPES) {
      es.addEventListener(type, (m) => this.apply(JSON.parse((m as MessageEvent).data) as KaEvent));
    }
  }

  private async resync(): Promise<void> {
    try {
      await this.snapshot();
    } catch {
      /* the stream's reconnect will try again */
    }
  }

  async refreshBoards(): Promise<void> {
    try {
      this.boards.set((await this.api.get<{ boards: BoardOut[] }>('/boards')).body.boards ?? []);
    } catch {
      /* keep the old list */
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
    switch (e.type) {
      case 'store.changed':
        if (e.data?.what === 'boards') {
          void this.refreshBoards();
          break;
        }
        void this.resync();
        return;
      case 'reset':
        void this.resync();
        return;
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
    if (e.ticket) this.touched.set({ id: e.ticket, actor: e.actor, at: Date.now() });
    if (e.type !== 'presence.changed' || e.data?.state !== 'active') {
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
