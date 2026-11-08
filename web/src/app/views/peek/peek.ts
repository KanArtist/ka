import { ChangeDetectionStrategy, Component, ElementRef, HostListener, computed, effect, inject, input, output, signal, untracked, viewChild } from '@angular/core';
import { Router, RouterLink } from '@angular/router';
import { marked } from 'marked';
import { Api, ApiError, CommentOut, Detail, KaEvent } from '../../api/api';
import { Actions } from '../../actions';
import { Live } from '../../live';
import { Actor, isAgent } from '../../ui/actor';
import { Menu, MenuOption } from '../../ui/menu';
import { Clock, ago, exact } from '../../ui/time';
import { describe } from '../activity/describe';

interface Entry {
  key: string;
  at: string;
  actor: string;
  verb: string;
  tone?: string;
  comment?: CommentOut;
  html?: string;
  commit?: string;
  store?: string;
}

type MenuKind = 'status' | 'priority' | 'kind' | 'assignee' | 'labels';

interface CommitView {
  sha: string;
  subject: string;
  diff: string;
  truncated?: boolean;
}

/**
 * The ticket peek (PRD §13.2): properties, the rendered description,
 * acceptance, relations, and a timeline that merges comments with the
 * git-derived changes, each linked to the commit behind it. It follows the
 * ticket live.
 */
@Component({
  selector: 'ka-peek',
  imports: [RouterLink, Actor, Menu],
  changeDetection: ChangeDetectionStrategy.OnPush,
  templateUrl: './peek.html',
  styleUrl: './peek.css',
})
export class Peek {
  readonly id = input.required<string>();
  /** In the side panel (true) or as its own page (false). */
  readonly panel = input(true);
  readonly close = output<void>();

  private api = inject(Api);
  private router = inject(Router);
  protected live = inject(Live);
  private clock = inject(Clock);
  protected actions = inject(Actions);

  protected readonly detail = signal<Detail | null>(null);
  /** The ETag of the loaded version, sent as If-Match when saving the body. */
  private etag = '';
  /** The ETag when editing began: live reloads must not move it. */
  private editEtag = '';
  protected readonly history = signal<KaEvent[]>([]);
  protected readonly error = signal<ApiError | null>(null);
  protected readonly openCommit = signal<string>('');
  protected readonly commit = signal<CommitView | null>(null);
  protected readonly commitError = signal('');

  protected readonly now = this.clock.now;
  protected readonly ago = ago;
  protected readonly exact = exact;
  protected readonly isAgent = isAgent;

  protected readonly description = computed(() => render(this.detail()?.body));
  protected readonly category = computed(() => {
    const d = this.detail();
    return d ? this.live.category(d) : '';
  });
  protected readonly acceptance = computed(() => {
    const items = this.detail()?.acceptance_items ?? [];
    return { items, done: items.filter((i) => i.done).length };
  });
  protected readonly fields = computed(() => Object.entries(this.detail()?.fields ?? {}));

  /** Comments plus changes from git, oldest first. */
  protected readonly timeline = computed<Entry[]>(() => {
    const d = this.detail();
    if (!d) return [];
    const commitByComment = new Map<string, KaEvent>();
    const out: Entry[] = [];
    for (const e of this.history()) {
      if (e.type === 'comment.created') {
        if (e.data?.comment?.name) commitByComment.set(e.data.comment.name, e);
        continue;
      }
      const line = describe(e);
      out.push({ key: `${e.commit}:${e.type}`, at: e.at, actor: e.actor || 'someone', verb: line.verb, tone: line.tone, commit: e.commit, store: e.store });
    }
    for (const c of d.comments ?? []) {
      const ev = commitByComment.get(c.name);
      const line = describe({ id: 0, type: 'comment.created', at: c.created, data: { comment: c } });
      out.push({ key: 'c:' + c.name, at: c.created, actor: c.author, verb: line.verb, tone: line.tone, comment: c, html: render(c.body), commit: ev?.commit, store: ev?.store });
    }
    // Closing makes a move and a resolution comment; keep the comment.
    out.sort((a, b) => a.at.localeCompare(b.at) || (a.comment ? 1 : -1));
    return out.filter((e, i) => {
      const next = out[i + 1];
      return !(e.commit && !e.comment && next?.comment?.type === 'resolution' && next.commit === e.commit);
    });
  });

  private reloadTimer?: ReturnType<typeof setTimeout>;

  // ---- editing ----
  protected readonly menu = signal<{ kind: MenuKind; anchor: HTMLElement } | null>(null);
  protected readonly editingTitle = signal(false);
  protected readonly editingBody = signal(false);
  protected readonly bodyDraft = signal('');
  protected readonly bodyConflict = signal(false);
  protected readonly bodyError = signal('');
  protected readonly saving = signal(false);
  protected readonly commentText = signal('');
  protected readonly commentType = signal('note');
  private statusBtn = viewChild<ElementRef<HTMLElement>>('statusBtn');
  private priorityBtn = viewChild<ElementRef<HTMLElement>>('priorityBtn');
  private assigneeBtn = viewChild<ElementRef<HTMLElement>>('assigneeBtn');
  private labelsBtn = viewChild<ElementRef<HTMLElement>>('labelsBtn');
  private kindBtn = viewChild<ElementRef<HTMLElement>>('kindBtn');
  private commentBox = viewChild<ElementRef<HTMLTextAreaElement>>('commentBox');

  protected readonly projectInfo = computed(() => {
    const d = this.detail();
    return d ? this.live.schema()?.projects?.find((p) => p.key === d.project) : undefined;
  });
  protected readonly writable = computed(() => {
    const p = this.projectInfo();
    return !!p && !p.read_only && !p.archived;
  });
  protected readonly claimedByMe = computed(() => this.detail()?.claim?.by === this.live.you());

  protected readonly menuOptions = computed<MenuOption[]>(() => {
    const m = this.menu();
    const d = this.detail();
    const p = this.projectInfo();
    const sch = this.live.schema();
    if (!m || !d) return [];
    switch (m.kind) {
      case 'status':
        return (p?.statuses ?? []).map((s) => ({ value: s.name, label: s.name, cat: s.category, hint: s.category }));
      case 'priority':
        return [...(sch?.priorities ?? ['P0', 'P1', 'P2', 'P3']).map((x) => ({ value: x, label: x, prio: x, hint: PRIO_HINT[x] })), { value: '', label: 'No priority' }];
      case 'kind':
        return [...(p?.kinds ?? []).map((k) => ({ value: k, label: k })), { value: '', label: 'No kind' }];
      case 'assignee': {
        const you = this.live.you();
        const people = [you, ...(sch?.humans ?? []).filter((h) => h !== you), ...(sch?.agents ?? []), ...(sch?.roles ?? [])].filter(Boolean);
        return [{ value: '', label: 'Anyone' }, ...[...new Set(people)].map((a) => ({ value: a, label: a === you ? `${a} (you)` : a, actor: a.startsWith('role/') ? undefined : a }))];
      }
      case 'labels': {
        const used = new Set<string>(p?.labels ?? []);
        for (const t of this.live.tickets().values()) if (t.project === d.project) for (const l of t.labels ?? []) used.add(l);
        for (const l of d.labels ?? []) used.add(l);
        return [...used].sort().map((l) => ({ value: l, label: l }));
      }
    }
  });
  protected readonly menuSelected = computed<string | string[]>(() => {
    const m = this.menu();
    const d = this.detail();
    if (!m || !d) return '';
    if (m.kind === 'labels') return d.labels ?? [];
    return (d as any)[m.kind] ?? '';
  });

  constructor() {
    effect(() => {
      const id = this.id();
      untracked(() => {
        this.detail.set(null);
        this.error.set(null);
        this.openCommit.set('');
        void this.load(id);
      });
    });
    // Follow the ticket live: reload when an event touches it.
    effect(() => {
      const t = this.live.touched();
      if (t && t.id === untracked(this.id)) {
        clearTimeout(this.reloadTimer);
        this.reloadTimer = setTimeout(() => void this.load(t.id), 200);
      }
    });
  }

  private async load(id: string): Promise<void> {
    try {
      const [d, h] = await Promise.all([
        this.api.get<Detail>(`/tickets/${encodeURIComponent(id)}?include=comments`),
        this.api.get<{ events: KaEvent[] }>(`/activity?ticket=${encodeURIComponent(id)}&limit=300`),
      ]);
      if (id !== this.id()) return;
      this.detail.set(d.body);
      this.etag = d.etag ?? '';
      this.history.set(h.body.events ?? []);
    } catch (e) {
      if (id === this.id()) this.error.set(e instanceof ApiError ? e : new ApiError(0, { detail: String(e) }));
    }
  }

  protected async toggleCommit(e: Entry): Promise<void> {
    if (!e.commit) return;
    if (this.openCommit() === e.key) {
      this.openCommit.set('');
      return;
    }
    this.openCommit.set(e.key);
    this.commit.set(null);
    this.commitError.set('');
    try {
      const q = e.store ? `?store=${encodeURIComponent(e.store)}` : '';
      const r = await this.api.get<CommitView>(`/commits/${e.commit}${q}`);
      this.commit.set(r.body);
    } catch (err: any) {
      this.commitError.set(err?.message ?? String(err));
    }
  }

  protected diffLines(diff: string): { cls: string; text: string }[] {
    return diff.split('\n').map((text) => ({
      text,
      cls: text.startsWith('+++') || text.startsWith('---') ? 'meta' : text.startsWith('+') ? 'add' : text.startsWith('-') ? 'del' : text.startsWith('@@') ? 'hunk' : text.startsWith('diff ') ? 'file' : '',
    }));
  }

  protected expand(): void {
    void this.router.navigate(['/ticket', this.id()]);
  }

  protected openMenu(kind: MenuKind, anchor: HTMLElement | undefined): void {
    if (!anchor || !this.writable()) return;
    this.menu.set({ kind, anchor });
  }

  protected async pick(value: string): Promise<void> {
    const m = this.menu();
    const d = this.detail();
    if (!m || !d) return;
    switch (m.kind) {
      case 'status':
        if (await this.actions.transition(d, value)) this.refresh();
        return;
      case 'labels': {
        const on = (d.labels ?? []).includes(value);
        // Optimistic: reflect the toggle now; the event stream confirms it.
        this.detail.set({ ...d, labels: on ? (d.labels ?? []).filter((l) => l !== value) : [...(d.labels ?? []), value] });
        if (!(await this.actions.addRemove(d.id, 'labels', on ? [] : [value], on ? [value] : []))) this.refresh();
        return;
      }
      default:
        if (((d as any)[m.kind] ?? '') === value) return;
        if (await this.actions.set(d.id, { [m.kind]: value || null })) this.refresh();
    }
  }

  protected async saveTitle(v: string): Promise<void> {
    this.editingTitle.set(false);
    const d = this.detail();
    const t = v.trim();
    if (!d || !t || t === d.title) return;
    this.detail.set({ ...d, title: t });
    if (!(await this.actions.set(d.id, { title: t }))) this.refresh();
  }

  protected editBody(): void {
    if (!this.writable()) return;
    this.bodyDraft.set((this.detail()?.body ?? '').replace(/^\n+/, ''));
    this.editEtag = this.etag;
    this.bodyConflict.set(false);
    this.bodyError.set('');
    this.editingBody.set(true);
  }

  protected async saveBody(): Promise<void> {
    const d = this.detail();
    if (!d || this.saving()) return;
    this.saving.set(true);
    const r = await this.actions.saveBody(d.id, this.bodyDraft(), this.editEtag);
    this.saving.set(false);
    if (r.ok) {
      this.editingBody.set(false);
      this.refresh();
    } else if (r.conflict) {
      this.bodyConflict.set(true);
    } else {
      this.bodyError.set(r.error ?? 'Could not save');
    }
  }

  /** After a conflict: load their version (and its ETag), keeping the draft. */
  protected async takeTheirs(): Promise<void> {
    await this.load(this.id());
    this.editEtag = this.etag;
    this.bodyConflict.set(false);
  }

  protected bodyKey(e: KeyboardEvent): void {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      void this.saveBody();
    } else if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      this.editingBody.set(false);
    }
  }

  protected async toggleCheck(index: number, done: boolean): Promise<void> {
    const d = this.detail();
    if (!d || !this.writable()) return;
    const items = (d.acceptance_items ?? []).map((a, i) => (i === index ? { ...a, done } : a));
    this.detail.set({ ...d, acceptance_items: items });
    if (!(await this.actions.check(d.id, index, done))) this.refresh();
  }

  protected async claim(steal = false): Promise<void> {
    const d = this.detail();
    if (d && (await this.actions.claim(d.id, steal))) this.refresh();
  }

  protected async release(force = false): Promise<void> {
    const d = this.detail();
    if (d && (await this.actions.release(d.id, force))) this.refresh();
  }

  protected async postComment(): Promise<void> {
    const d = this.detail();
    const text = this.commentText().trim();
    if (!d || !text || this.saving()) return;
    this.saving.set(true);
    const ok = await this.actions.comment(d.id, text, this.commentType());
    this.saving.set(false);
    if (ok) {
      this.commentText.set('');
      this.commentBox()?.nativeElement.blur();
      this.refresh();
    }
  }

  protected commentKey(e: KeyboardEvent): void {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      void this.postComment();
    } else if (e.key === 'Escape') {
      (e.target as HTMLElement).blur();
    }
  }

  private refresh(): void {
    clearTimeout(this.reloadTimer);
    this.reloadTimer = setTimeout(() => void this.load(this.id()), 60);
  }

  /** Linear-style shortcuts while a ticket is open: s p a l k e m. */
  @HostListener('document:keydown', ['$event'])
  protected onKey(e: KeyboardEvent): void {
    if (e.metaKey || e.ctrlKey || e.altKey || e.defaultPrevented || this.menu() || this.editingBody() || this.editingTitle()) return;
    if (this.actions.closing() || this.actions.creating() || isTyping(e) || document.querySelector('ka-palette')) return;
    const map: Record<string, () => void> = {
      s: () => this.openMenu('status', this.statusBtn()?.nativeElement),
      p: () => this.openMenu('priority', this.priorityBtn()?.nativeElement),
      a: () => this.openMenu('assignee', this.assigneeBtn()?.nativeElement),
      l: () => this.openMenu('labels', this.labelsBtn()?.nativeElement),
      k: () => this.openMenu('kind', this.kindBtn()?.nativeElement),
      e: () => this.editBody(),
      m: () => this.commentBox()?.nativeElement.focus(),
    };
    const f = map[e.key];
    if (f && this.detail() && this.writable()) {
      e.preventDefault();
      f();
    }
  }

  /** A related ticket's status category, when it is in the workspace. */
  protected relCat(id: string): string {
    const t = this.live.tickets().get(id);
    return t ? this.live.category(t) : '';
  }
}

const PRIO_HINT: Record<string, string> = { P0: 'urgent', P1: 'high', P2: 'normal', P3: 'someday' };

function isTyping(e: KeyboardEvent): boolean {
  const t = e.target as HTMLElement | null;
  return !!t && (t.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(t.tagName));
}

/** Markdown to HTML. Angular's [innerHTML] sanitizer strips scripts, handlers and unsafe URLs. */
function render(md: string | undefined): string {
  if (!md?.trim()) return '';
  return marked.parse(md, { gfm: true, async: false }) as string;
}
