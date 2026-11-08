import {
  ChangeDetectionStrategy,
  Component,
  ElementRef,
  afterRenderEffect,
  computed,
  effect,
  inject,
  signal,
  untracked,
  viewChild,
} from '@angular/core';
import { RouterLink } from '@angular/router';
import { Summary } from '../../api/api';
import { Live } from '../../live';
import { Avatar, actorName, isAgent } from '../../ui/actor';
import { Clock, ago, exact } from '../../ui/time';

type Lane = 'none' | 'project' | 'assignee' | 'priority' | 'parent';

interface Column {
  status: string;
  category: string;
  cards: Summary[];
  hidden: number; // cards beyond the cap, or older closed ones
}

interface Row {
  key: string;
  label: string;
  agent?: boolean;
  columns: Column[];
  count: number;
}

/** Cards shown per column before "N more". */
const COLUMN_CAP = 60;
/** Closed columns show only recently updated tickets. */
const CLOSED_WINDOW_MS = 14 * 86_400_000;
const PRIORITY_ORDER = ['P0', 'P1', 'P2', 'P3'];

/**
 * The board (PRD §13.2), read-only in M2: columns by status, optional
 * swimlanes, and cards with priority, kind, labels, claim age, acceptance
 * progress and blocked state. When an event moves a card, it glides to
 * its new column wearing the actor's mark for a moment.
 */
@Component({
  selector: 'ka-board',
  imports: [RouterLink, Avatar],
  changeDetection: ChangeDetectionStrategy.OnPush,
  templateUrl: './board.html',
  styleUrl: './board.css',
})
export class Board {
  protected live = inject(Live);
  private clock = inject(Clock);
  private host = viewChild.required<ElementRef<HTMLElement>>('host');

  protected readonly projects = computed(() => (this.live.schema()?.projects ?? []).filter((p) => !p.archived));
  protected readonly project = signal('');
  protected readonly lane = signal<Lane>('none');
  protected readonly filter = signal('');
  protected readonly showCanceled = signal(false);
  protected readonly now = this.clock.now;
  protected readonly ago = ago;
  protected readonly exact = exact;
  protected readonly isAgent = isAgent;
  protected readonly actorName = actorName;

  /** Cards that just moved: id -> actor, for the arrival mark. */
  protected readonly moved = signal<ReadonlyMap<string, string>>(new Map());
  private rects = new Map<string, DOMRect>();

  /** The statuses to show as columns, in workflow order. */
  private readonly statuses = computed(() => {
    const ps = this.projects();
    const chosen = this.project() ? ps.filter((p) => p.key === this.project()) : ps;
    const out: { name: string; category: string }[] = [];
    for (const p of chosen) {
      for (const s of p.statuses ?? []) {
        if (!out.some((o) => o.name === s.name)) out.push({ name: s.name, category: s.category });
      }
    }
    return out.filter((s) => this.showCanceled() || s.category !== 'canceled');
  });

  private readonly tickets = computed(() => {
    const project = this.project();
    const q = this.filter().trim().toLowerCase();
    const out: Summary[] = [];
    for (const t of this.live.tickets().values()) {
      if (project && t.project !== project) continue;
      if (q && !t.id.toLowerCase().includes(q) && !t.title.toLowerCase().includes(q) && !(t.labels ?? []).some((l) => l.toLowerCase().includes(q))) {
        continue;
      }
      out.push(t);
    }
    return out;
  });

  protected readonly rows = computed<Row[]>(() => {
    const statuses = this.statuses();
    const lane = this.lane();
    const now = this.now();
    const groups = new Map<string, Summary[]>();
    for (const t of this.tickets()) {
      const k = laneKey(t, lane);
      let g = groups.get(k);
      if (!g) groups.set(k, (g = []));
      g.push(t);
    }
    const keys = [...groups.keys()].sort((a, b) => laneOrder(a, lane) - laneOrder(b, lane) || a.localeCompare(b));
    // An empty triage column is noise; every other column stays, so the
    // workflow reads left to right.
    const visible = statuses.filter((s) => s.category !== 'triage' || this.tickets().some((t) => t.status === s.name));
    return keys.map((key) => {
      const list = groups.get(key)!;
      const columns: Column[] = visible.map((s) => {
        let cards = list.filter((t) => t.status === s.name);
        let hidden = 0;
        if (s.category === 'completed' || s.category === 'canceled') {
          const recent = cards.filter((t) => now - Date.parse(t.updated ?? '') < CLOSED_WINDOW_MS);
          hidden = cards.length - recent.length;
          cards = recent;
        }
        cards.sort(byPriority);
        if (cards.length > COLUMN_CAP) {
          hidden += cards.length - COLUMN_CAP;
          cards = cards.slice(0, COLUMN_CAP);
        }
        return { status: s.name, category: s.category, cards, hidden };
      });
      return { key, label: this.laneLabel(key, lane), agent: lane === 'assignee' && isAgent(key), columns, count: list.length };
    });
  });

  protected readonly columnTotals = computed(() => {
    const totals = new Map<string, number>();
    for (const r of this.rows()) for (const c of r.columns) totals.set(c.status, (totals.get(c.status) ?? 0) + c.cards.length + c.hidden);
    return totals;
  });

  constructor() {
    const ws = this.live.workspace();
    untracked(() => this.project.set(ws?.default_project ?? (this.live.schema()?.projects?.length === 1 ? this.live.schema()!.projects![0].key : '')));

    // A ticket an event touched gets the actor's mark for a few seconds.
    effect(() => {
      const t = this.live.touched();
      if (!t || !t.actor) return;
      untracked(() => {
        const m = new Map(this.moved());
        m.set(t.id, t.actor!);
        this.moved.set(m);
        setTimeout(() => {
          const n = new Map(this.moved());
          if (n.get(t.id) === t.actor) n.delete(t.id);
          this.moved.set(n);
        }, 3200);
      });
    });

    // FLIP: cards whose position changed glide from where they were.
    afterRenderEffect(() => {
      this.rows();
      const reduce = matchMedia('(prefers-reduced-motion: reduce)').matches;
      const next = new Map<string, DOMRect>();
      for (const el of this.host().nativeElement.querySelectorAll<HTMLElement>('[data-card]')) {
        const id = el.dataset['card']!;
        const r = el.getBoundingClientRect();
        next.set(id, r);
        const prev = this.rects.get(id);
        if (!reduce && prev && untracked(this.moved).has(id) && (Math.abs(prev.left - r.left) > 2 || Math.abs(prev.top - r.top) > 2)) {
          el.animate(
            [{ transform: `translate(${prev.left - r.left}px, ${prev.top - r.top}px)` }, { transform: 'none' }],
            { duration: 520, easing: 'cubic-bezier(0.2, 0.7, 0.2, 1)' },
          );
        }
      }
      this.rects = next;
    });
  }

  protected acceptance(t: Summary): { done: number; total: number } | null {
    if (!t.acceptance) return null;
    const [d, n] = t.acceptance.split('/').map(Number);
    return n ? { done: d, total: n } : null;
  }

  private laneLabel(key: string, lane: Lane): string {
    if (lane === 'none') return '';
    if (key === '') {
      return { project: 'No project', assignee: 'Unassigned', priority: 'No priority', parent: 'No parent', none: '' }[lane];
    }
    if (lane === 'parent') {
      const p = this.live.tickets().get(key);
      return p ? `${key}  ${p.title}` : key;
    }
    if (lane === 'assignee') return actorName(key);
    return key;
  }

  protected trackCard = (_: number, t: Summary) => t.id;
}

function laneKey(t: Summary, lane: Lane): string {
  switch (lane) {
    case 'project':
      return t.project;
    case 'assignee':
      return t.assignee ?? t.claim?.by ?? '';
    case 'priority':
      return t.priority ?? '';
    case 'parent':
      return t.parent ?? '';
    default:
      return '';
  }
}

function laneOrder(k: string, lane: Lane): number {
  if (lane === 'priority') {
    const i = PRIORITY_ORDER.indexOf(k);
    return i < 0 ? 99 : i;
  }
  return k === '' ? 99 : 0; // the empty lane last
}

function byPriority(a: Summary, b: Summary): number {
  const pa = PRIORITY_ORDER.indexOf(a.priority ?? '');
  const pb = PRIORITY_ORDER.indexOf(b.priority ?? '');
  return (pa < 0 ? 9 : pa) - (pb < 0 ? 9 : pb) || (b.updated ?? '').localeCompare(a.updated ?? '');
}
