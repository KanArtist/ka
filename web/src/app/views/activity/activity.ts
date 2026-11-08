import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { RouterLink } from '@angular/router';
import { Api, KaEvent } from '../../api/api';
import { Live } from '../../live';
import { Actor, isAgent } from '../../ui/actor';
import { Clock, ago, exact } from '../../ui/time';
import { Kind, Line, describe, kindOf } from './describe';

interface Item {
  key: string;
  e: KaEvent;
  kind: Kind;
  line: Line;
  fresh: boolean; // arrived live while this view was open
}

interface Group {
  key: string;
  actor: string;
  ticket?: string;
  store?: string;
  latest: string;
  items: Item[]; // oldest first
  day: string;
}

const KINDS: { id: Kind; label: string }[] = [
  { id: 'tickets', label: 'Tickets' },
  { id: 'comments', label: 'Comments' },
  { id: 'claims', label: 'Claims' },
  { id: 'questions', label: 'Questions' },
  { id: 'sessions', label: 'Sessions' },
  { id: 'sync', label: 'Sync' },
];

/** Consecutive events by one actor on one ticket within this window form a group. */
const GROUP_WINDOW_MS = 20 * 60 * 1000;

/**
 * The watch view (PRD §13.2): what agents and people did, live, grouped by
 * who did it to which ticket. History comes from git (GET /activity); new
 * events arrive over SSE and are highlighted in the actor's ink.
 */
@Component({
  selector: 'ka-activity',
  imports: [RouterLink, Actor],
  changeDetection: ChangeDetectionStrategy.OnPush,
  templateUrl: './activity.html',
  styleUrl: './activity.css',
})
export class Activity {
  private api = inject(Api);
  protected live = inject(Live);
  private clock = inject(Clock);
  private readonly mountedAt = Date.now();
  private readonly mountedSeq = Math.max(0, ...this.live.events().map((e) => e.id));

  protected readonly kinds = KINDS;
  protected readonly history = signal<KaEvent[]>([]);
  protected readonly more = signal(false);
  protected readonly loadingOlder = signal(false);
  protected readonly error = signal('');

  protected readonly project = signal('');
  protected readonly who = signal<'all' | 'agents' | 'people'>('all');
  protected readonly hidden = signal<ReadonlySet<Kind>>(new Set<Kind>(['sync', 'sessions']));

  protected readonly projects = computed(() => this.live.workspace()?.projects ?? []);

  private readonly items = computed<Item[]>(() => {
    const live = this.live.events().filter((e) => kindOf(e) && !(e.type === 'presence.changed' && e.data?.state === 'active'));
    const hist = this.history();
    // Live events that the history page already contains are dropped.
    const seen = new Set(hist.map(fuzzyKey));
    const out: Item[] = [];
    for (const e of live) {
      if (seen.has(fuzzyKey(e))) continue;
      out.push(this.item(e, 'e' + e.id, e.id > this.mountedSeq));
    }
    for (const e of hist) {
      const k = kindOf(e);
      if (!k) continue;
      out.push(this.item(e, `h${e.commit}:${e.type}:${e.ticket ?? ''}`, false));
    }
    out.sort((a, b) => (b.e.at ?? '').localeCompare(a.e.at ?? ''));
    return out;
  });

  protected readonly groups = computed<Group[]>(() => {
    const project = this.project();
    const who = this.who();
    const hidden = this.hidden();
    const groups: Group[] = [];
    let cur: Group | undefined;
    for (const it of this.items()) {
      const e = it.e;
      if (hidden.has(it.kind)) continue;
      if (project && e.project && e.project !== project) continue;
      const actor = e.actor || 'someone';
      if (who === 'agents' && !isAgent(actor)) continue;
      if (who === 'people' && isAgent(actor)) continue;
      const near = cur && Date.parse(cur.items[0].e.at) - Date.parse(e.at) <= GROUP_WINDOW_MS;
      if (cur && near && cur.actor === actor && cur.ticket === e.ticket && (e.ticket || cur.store === e.store)) {
        cur.items.unshift(it);
        continue;
      }
      cur = { key: it.key, actor, ticket: e.ticket, store: e.store, latest: e.at, items: [it], day: dayLabel(e.at) };
      groups.push(cur);
    }
    for (const g of groups) g.items = dropRedundant(g.items);
    return groups;
  });

  protected readonly now = this.clock.now;
  protected readonly ago = ago;
  protected readonly exact = exact;

  constructor() {
    void this.load();
  }

  private item(e: KaEvent, key: string, live: boolean): Item {
    return { key, e, kind: kindOf(e)!, line: describe(e), fresh: live && Date.now() - this.mountedAt > 500 };
  }

  private async load(before?: string): Promise<void> {
    try {
      const q = before ? `?limit=150&before=${encodeURIComponent(before)}` : '?limit=150';
      const r = await this.api.get<{ events: KaEvent[]; more: boolean }>('/activity' + q);
      this.history.update((h) => (before ? [...h, ...(r.body.events ?? [])] : (r.body.events ?? [])));
      this.more.set(r.body.more);
    } catch (e: any) {
      this.error.set(e?.message ?? String(e));
    }
  }

  protected async older(): Promise<void> {
    const h = this.history();
    if (!h.length) return;
    this.loadingOlder.set(true);
    await this.load(h[h.length - 1].at);
    this.loadingOlder.set(false);
  }

  protected toggle(k: Kind): void {
    this.hidden.update((s) => {
      const n = new Set(s);
      if (n.has(k)) n.delete(k);
      else n.add(k);
      return n;
    });
  }

  protected title(g: Group): string {
    if (!g.ticket) return '';
    const t = this.live.tickets().get(g.ticket);
    if (t) return t.title;
    for (const it of g.items) {
      const d = it.e.data;
      const title = d?.ticket?.title;
      if (title) return title;
    }
    return '';
  }

  protected category(g: Group): string {
    const t = g.ticket ? this.live.tickets().get(g.ticket) : undefined;
    return t ? this.live.category(t) : '';
  }

  protected isAgent = isAgent;
  protected newDay(i: number, groups: Group[]): boolean {
    return i === 0 || groups[i - 1].day !== groups[i].day;
  }
}

/**
 * Closing a ticket is a status change plus a resolution comment; "closed it
 * as done" with the summary says both, so the bare move line is dropped.
 */
function dropRedundant(items: Item[]): Item[] {
  return items.filter((it, i) => {
    if (it.e.type !== 'ticket.moved') return true;
    const next = items[i + 1] ?? items[i - 1];
    return !(
      next &&
      next.e.type === 'comment.created' &&
      next.e.data?.comment?.type === 'resolution' &&
      Math.abs(Date.parse(next.e.at) - Date.parse(it.e.at)) < 10_000
    );
  });
}

function fuzzyKey(e: KaEvent): string {
  // History times come from the commit; live times from the index refresh.
  // Same type, ticket and actor within the same 10 seconds is the same event.
  return `${e.type}|${e.ticket ?? ''}|${e.actor ?? ''}|${Math.floor(Date.parse(e.at) / 10_000)}`;
}

function dayLabel(iso: string): string {
  const d = new Date(iso);
  const today = new Date();
  const yesterday = new Date(Date.now() - 86_400_000);
  if (d.toDateString() === today.toDateString()) return 'Today';
  if (d.toDateString() === yesterday.toDateString()) return 'Yesterday';
  return d.toLocaleDateString(undefined, { weekday: 'long', month: 'long', day: 'numeric' });
}
