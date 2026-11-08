import { ChangeDetectionStrategy, Component, ElementRef, computed, effect, inject, signal, viewChild } from '@angular/core';
import { Router } from '@angular/router';
import { Summary } from '../../api/api';
import { Actions } from '../../actions';
import { Live } from '../../live';
import { Query, QueryBar } from '../../query';
import { Avatar, actorName, isAgent } from '../../ui/actor';
import { Menu, MenuOption } from '../../ui/menu';
import { Clock, ago, exact } from '../../ui/time';

type Group = 'none' | 'status' | 'project' | 'assignee' | 'priority' | 'kind' | 'parent';

interface Col {
  key: string; // id, title, status, ..., or fields.<name>
  label: string;
  width: string;
  /** Editable inline through a menu. */
  edit?: 'status' | 'priority' | 'kind' | 'assignee' | 'labels';
}

interface Section {
  key: string;
  label: string;
  cat?: string;
  rows: Summary[];
  total: number;
}

const BASE_COLS: Col[] = [
  { key: 'id', label: 'ID', width: '104px' },
  { key: 'title', label: 'Title', width: 'minmax(240px, 1fr)' },
  { key: 'status', label: 'Status', width: '128px', edit: 'status' },
  { key: 'priority', label: 'Priority', width: '76px', edit: 'priority' },
  { key: 'kind', label: 'Kind', width: '92px', edit: 'kind' },
  { key: 'assignee', label: 'Assignee', width: '150px', edit: 'assignee' },
  { key: 'claim', label: 'Claim', width: '150px' },
  { key: 'labels', label: 'Labels', width: '180px', edit: 'labels' },
  { key: 'acceptance', label: 'Acceptance', width: '96px' },
  { key: 'project', label: 'Project', width: '96px' },
  { key: 'parent', label: 'Parent', width: '104px' },
  { key: 'owner', label: 'Owner', width: '130px' },
  { key: 'created', label: 'Created', width: '84px' },
  { key: 'updated', label: 'Updated', width: '84px' },
];
const DEFAULT_COLS = ['id', 'title', 'status', 'priority', 'kind', 'assignee', 'labels', 'updated'];
const PRIORITY_ORDER = ['P0', 'P1', 'P2', 'P3'];
/** Rows rendered per section before "Show more". */
const PAGE = 300;
const STORE_KEY = 'ka.table';

/**
 * The table (PRD §13.2, GitHub Projects style): configurable columns
 * including custom fields, grouping, sorting by any column, and inline
 * edits, over the query shared with the board.
 */
@Component({
  selector: 'ka-table',
  imports: [Avatar, QueryBar, Menu],
  changeDetection: ChangeDetectionStrategy.OnPush,
  templateUrl: './table.html',
  styleUrl: './table.css',
})
export class Table {
  protected live = inject(Live);
  private query = inject(Query);
  private actions = inject(Actions);
  private router = inject(Router);
  private clock = inject(Clock);
  private host = viewChild.required<ElementRef<HTMLElement>>('host');

  protected readonly now = this.clock.now;
  protected readonly ago = ago;
  protected readonly exact = exact;
  protected readonly isAgent = isAgent;
  protected readonly actorName = actorName;

  private saved = load();
  protected readonly visible = signal<string[]>(this.saved.cols ?? DEFAULT_COLS);
  protected readonly group = signal<Group>(this.saved.group ?? 'status');
  protected readonly sortKey = signal<string>(this.saved.sort ?? 'priority');
  protected readonly sortDesc = signal<boolean>(this.saved.desc ?? false);
  protected readonly collapsed = signal<ReadonlySet<string>>(new Set(this.saved.collapsed ?? []));
  protected readonly shown = signal<ReadonlyMap<string, number>>(new Map());
  protected readonly focusId = signal('');

  protected readonly menu = signal<{ kind: string; anchor: HTMLElement; ticket?: Summary } | null>(null);

  /** Every column: the built-ins plus each custom field declared in any project. */
  protected readonly allCols = computed<Col[]>(() => {
    const fields = new Map<string, string>();
    for (const p of this.live.schema()?.projects ?? []) for (const [k, d] of Object.entries(p.fields ?? {})) fields.set(k, (d as any)?.type ?? '');
    return [...BASE_COLS, ...[...fields.keys()].sort().map((k) => ({ key: 'fields.' + k, label: k, width: '140px' }))];
  });
  protected readonly cols = computed(() => {
    const all = new Map(this.allCols().map((c) => [c.key, c]));
    return this.visible().map((k) => all.get(k)).filter((c): c is Col => !!c);
  });
  protected readonly template = computed(() => this.cols().map((c) => c.width).join(' '));

  private readonly rows = computed(() => {
    const match = this.query.matches();
    const out: Summary[] = [];
    for (const t of this.live.tickets().values()) if (match(t)) out.push(t);
    const key = this.sortKey();
    const dir = this.sortDesc() ? -1 : 1;
    out.sort((a, b) => dir * compare(this.value(a, key), this.value(b, key), key) || (b.updated ?? '').localeCompare(a.updated ?? ''));
    return out;
  });

  protected readonly total = computed(() => this.rows().length);

  protected readonly sections = computed<Section[]>(() => {
    const g = this.group();
    const rows = this.rows();
    if (g === 'none') return [{ key: '', label: '', rows, total: rows.length }];
    const map = new Map<string, Summary[]>();
    for (const t of rows) {
      const k = groupKey(t, g);
      let l = map.get(k);
      if (!l) map.set(k, (l = []));
      l.push(t);
    }
    const order = this.groupOrder(g);
    return [...map.entries()]
      .sort((a, b) => order(a[0]) - order(b[0]) || a[0].localeCompare(b[0]))
      .map(([k, l]) => ({
        key: k,
        label: this.groupLabel(k, g),
        cat: g === 'status' ? this.statusCat(k) : undefined,
        rows: l,
        total: l.length,
      }));
  });

  constructor() {
    // Closed groups start collapsed the first time.
    if (!this.saved.collapsed) this.collapsed.set(new Set(['status:done', 'status:canceled']));
    effect(() => {
      save({ cols: this.visible(), group: this.group(), sort: this.sortKey(), desc: this.sortDesc(), collapsed: [...this.collapsed()] });
    });
  }

  // ---- values ----

  protected value(t: Summary, key: string): any {
    if (key.startsWith('fields.')) return t.fields?.[key.slice(7)];
    switch (key) {
      case 'claim':
        return t.claim?.by ?? '';
      case 'labels':
        return (t.labels ?? []).join(', ');
      default:
        return (t as any)[key] ?? '';
    }
  }

  protected fieldText(v: unknown): string {
    if (v === undefined || v === null) return '';
    return Array.isArray(v) ? v.join(', ') : String(v);
  }

  protected cat(t: Summary): string {
    return this.live.category(t);
  }

  private statusCat(status: string): string {
    for (const p of this.live.schema()?.projects ?? []) {
      const s = p.statuses?.find((x) => x.name === status);
      if (s) return s.category;
    }
    return '';
  }

  private groupOrder(g: Group): (k: string) => number {
    if (g === 'status') {
      const names: string[] = [];
      for (const p of this.live.schema()?.projects ?? []) for (const s of p.statuses ?? []) if (!names.includes(s.name)) names.push(s.name);
      return (k) => (names.indexOf(k) + 1 || 99);
    }
    if (g === 'priority') return (k) => (PRIORITY_ORDER.indexOf(k) + 1 || 99);
    return (k) => (k === '' ? 99 : 0);
  }

  private groupLabel(k: string, g: Group): string {
    if (k === '') return { status: '', project: '', assignee: 'Unassigned', priority: 'No priority', kind: 'No kind', parent: 'No parent', none: '' }[g];
    if (g === 'parent') {
      const p = this.live.tickets().get(k);
      return p ? `${k} ${p.title}` : k;
    }
    return k;
  }

  // ---- interaction ----

  protected sortBy(key: string): void {
    if (this.sortKey() === key) this.sortDesc.update((d) => !d);
    else {
      this.sortKey.set(key);
      this.sortDesc.set(key === 'updated' || key === 'created');
    }
  }

  protected toggle(s: Section): void {
    const k = this.group() + ':' + s.key;
    this.collapsed.update((c) => {
      const n = new Set(c);
      if (n.has(k)) n.delete(k);
      else n.add(k);
      return n;
    });
  }

  protected isCollapsed(s: Section): boolean {
    return this.collapsed().has(this.group() + ':' + s.key);
  }

  protected limit(s: Section): number {
    return this.shown().get(s.key) ?? PAGE;
  }

  protected more(s: Section): void {
    this.shown.update((m) => new Map(m).set(s.key, this.limit(s) + PAGE));
  }

  protected open(t: Summary): void {
    this.focusId.set(t.id);
    void this.router.navigate([], { queryParams: { peek: t.id }, queryParamsHandling: 'merge' });
  }

  protected writable(t: Summary): boolean {
    const p = this.live.schema()?.projects?.find((x) => x.key === t.project);
    return !!p && !p.read_only && !p.archived;
  }

  protected editCell(e: Event, t: Summary, c: Col): void {
    e.stopPropagation();
    if (!c.edit || !this.writable(t)) {
      this.open(t);
      return;
    }
    this.menu.set({ kind: c.edit, anchor: e.currentTarget as HTMLElement, ticket: t });
  }

  protected openColumns(e: Event): void {
    this.menu.set({ kind: 'columns', anchor: e.currentTarget as HTMLElement });
  }

  protected readonly menuOptions = computed<MenuOption[]>(() => {
    const m = this.menu();
    if (!m) return [];
    if (m.kind === 'columns') return this.allCols().map((c) => ({ value: c.key, label: c.label, hint: c.key.startsWith('fields.') ? 'field' : '' }));
    const t = m.ticket!;
    const p = this.live.schema()?.projects?.find((x) => x.key === t.project);
    const sch = this.live.schema();
    switch (m.kind) {
      case 'status':
        return (p?.statuses ?? []).map((s) => ({ value: s.name, label: s.name, cat: s.category }));
      case 'priority':
        return [...(sch?.priorities ?? PRIORITY_ORDER).map((x) => ({ value: x, label: x, prio: x })), { value: '', label: 'No priority' }];
      case 'kind':
        return [...(p?.kinds ?? []).map((k) => ({ value: k, label: k })), { value: '', label: 'No kind' }];
      case 'assignee': {
        const you = this.live.you();
        const people = [you, ...(sch?.humans ?? []), ...(sch?.agents ?? []), ...(sch?.roles ?? [])].filter(Boolean);
        return [{ value: '', label: 'Anyone' }, ...[...new Set(people)].map((a) => ({ value: a, label: a === you ? `${a} (you)` : a, actor: a.startsWith('role/') ? undefined : a }))];
      }
      case 'labels': {
        const used = new Set<string>(p?.labels ?? []);
        for (const x of this.live.tickets().values()) if (x.project === t.project) for (const l of x.labels ?? []) used.add(l);
        return [...used].sort().map((l) => ({ value: l, label: l }));
      }
    }
    return [];
  });

  protected readonly menuSelected = computed<string | string[]>(() => {
    const m = this.menu();
    if (!m) return '';
    if (m.kind === 'columns') return this.visible();
    const t = this.live.tickets().get(m.ticket!.id) ?? m.ticket!;
    if (m.kind === 'labels') return t.labels ?? [];
    return (t as any)[m.kind] ?? '';
  });

  protected menuAllowsCustom(): boolean {
    const m = this.menu();
    if (m?.kind !== 'labels') return false;
    return !!this.live.schema()?.projects?.find((x) => x.key === m.ticket!.project)?.labels_open;
  }

  protected async pick(value: string): Promise<void> {
    const m = this.menu();
    if (!m) return;
    if (m.kind === 'columns') {
      this.visible.update((v) => (v.includes(value) ? v.filter((x) => x !== value) : [...v, value]));
      return;
    }
    const t = this.live.tickets().get(m.ticket!.id) ?? m.ticket!;
    if (m.kind === 'status') {
      const undo = this.live.patchTicket(t.id, { status: value });
      if (!(await this.actions.transition(t, value))) undo();
      return;
    }
    if (m.kind === 'labels') {
      const on = (t.labels ?? []).includes(value);
      const undo = this.live.patchTicket(t.id, { labels: on ? (t.labels ?? []).filter((l) => l !== value) : [...(t.labels ?? []), value] });
      if (!(await this.actions.addRemove(t.id, 'labels', on ? [] : [value], on ? [value] : []))) undo();
      return;
    }
    if (((t as any)[m.kind] ?? '') === value) return;
    const undo = this.live.patchTicket(t.id, { [m.kind]: value || undefined } as Partial<Summary>);
    if (!(await this.actions.set(t.id, { [m.kind]: value || null }))) undo();
  }

  /** j/k (or arrows) move between rows; Enter opens the focused row. */
  protected rowKey(e: KeyboardEvent, t: Summary): void {
    if (e.key === 'Enter') {
      e.preventDefault();
      this.open(t);
      return;
    }
    const down = e.key === 'j' || e.key === 'ArrowDown';
    const up = e.key === 'k' || e.key === 'ArrowUp';
    if (!down && !up) return;
    e.preventDefault();
    const rows = [...this.host().nativeElement.querySelectorAll<HTMLElement>('[data-row]')];
    const i = rows.findIndex((r) => r.dataset['row'] === t.id);
    rows[Math.max(0, Math.min(rows.length - 1, i + (down ? 1 : -1)))]?.focus();
  }

  protected trackRow = (_: number, t: Summary) => t.id;
}

function groupKey(t: Summary, g: Group): string {
  switch (g) {
    case 'assignee':
      return t.assignee ?? '';
    case 'parent':
      return t.parent ?? '';
    default:
      return ((t as any)[g] as string) ?? '';
  }
}

function compare(a: any, b: any, key: string): number {
  if (key === 'priority') {
    const pa = PRIORITY_ORDER.indexOf(a);
    const pb = PRIORITY_ORDER.indexOf(b);
    return (pa < 0 ? 9 : pa) - (pb < 0 ? 9 : pb);
  }
  if (key === 'acceptance') {
    const r = (s: string) => {
      const [d, n] = (s || '').split('/').map(Number);
      return n ? d / n : -1;
    };
    return r(a) - r(b);
  }
  const ea = a === '' || a === undefined || a === null;
  const eb = b === '' || b === undefined || b === null;
  if (ea || eb) return ea === eb ? 0 : ea ? 1 : -1; // empty last
  if (typeof a === 'number' && typeof b === 'number') return a - b;
  return String(a).localeCompare(String(b), undefined, { numeric: true });
}

interface Saved {
  cols?: string[];
  group?: Group;
  sort?: string;
  desc?: boolean;
  collapsed?: string[];
}

function load(): Saved {
  try {
    return JSON.parse(localStorage.getItem(STORE_KEY) ?? '{}');
  } catch {
    return {};
  }
}

function save(s: Saved): void {
  try {
    localStorage.setItem(STORE_KEY, JSON.stringify(s));
  } catch {
    /* private mode */
  }
}
