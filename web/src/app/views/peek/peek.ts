import { ChangeDetectionStrategy, Component, DestroyRef, ElementRef, HostListener, computed, effect, inject, input, output, signal, untracked, viewChild } from '@angular/core';
import { Router, RouterLink } from '@angular/router';
import { Api, ApiError, CommentOut, Detail, KaEvent } from '../../api/api';
import { Actions } from '../../actions';
import { Live } from '../../live';
import { Actor, actorName, isAgent } from '../../ui/actor';
import { KeysPipe } from '../../ui/keys';
import { FieldPipe, KindPipe, StatusPipe, categoryLabel, fieldLabel, kindLabel, statusLabel } from '../../ui/labels';
import { followInternalLink, renderMarkdown } from '../../ui/markdown';
import { Menu, MenuOption } from '../../ui/menu';
import { Clock, ago, exact } from '../../ui/time';
import { Toasts } from '../../ui/toast';
import { compactionSummary, describe } from '../activity/describe';
import { Hierarchy, isContainer } from '../../hierarchy';
import { ContainerPanel } from './container';

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
  /** store.compacted: the history ended here; the summary file to read. */
  compacted?: boolean;
  summary?: string;
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
 *
 * One instance shows one ticket: hosts key it by id (@for … track id), so
 * drafts, ETags and menus never carry over to another ticket.
 */
@Component({
  selector: 'ka-peek',
  imports: [RouterLink, Actor, Menu, KeysPipe, StatusPipe, KindPipe, FieldPipe, ContainerPanel],
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

  private toasts = inject(Toasts);
  private host = inject<ElementRef<HTMLElement>>(ElementRef);

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
  /** The open commit is not in git any more: its history was compacted. */
  protected readonly commitGone = signal(false);
  protected readonly diff = computed(() => {
    const c = this.commit();
    return c ? diffLines(c.diff) : [];
  });

  protected readonly now = this.clock.now;
  protected readonly ago = ago;
  protected readonly exact = exact;
  protected readonly isAgent = isAgent;

  /** Copies the ticket's reference (ID-slug) for pasting into chat or a commit. */
  protected copyRef(ref: string): void {
    navigator.clipboard?.writeText(ref).then(
      () => this.toasts.show('Copied ' + ref),
      () => this.toasts.show('Could not copy ' + ref, 'error'),
    );
  }

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
      if (e.type === 'store.compacted') {
        // The root commit holds the whole tree: no diff to show, but the summary says what came before.
        out.push({ key: `${e.commit}:${e.type}`, at: e.at, actor: e.actor || 'someone', verb: line.verb, tone: line.tone, store: e.store, compacted: true, summary: compactionSummary(e) });
        continue;
      }
      out.push({ key: `${e.commit}:${e.type}`, at: e.at, actor: e.actor || 'someone', verb: line.verb, tone: line.tone, commit: e.commit, store: e.store });
    }
    for (const c of d.comments ?? []) {
      const ev = commitByComment.get(c.name);
      const line = describe({ id: 0, type: 'comment.created', at: c.created, data: { comment: c } });
      out.push({ key: 'c:' + c.name, at: c.created, actor: c.author, verb: line.verb, tone: line.tone, comment: c, html: render(c.body), commit: ev?.commit, store: ev?.store });
    }
    // Closing makes a move and a resolution comment; keep the comment.
    out.sort((a, b) => a.at.localeCompare(b.at) || Number(!!a.comment) - Number(!!b.comment));
    return out.filter((e, i) => {
      const next = out[i + 1];
      return !(e.commit && !e.comment && next?.comment?.type === 'resolution' && next.commit === e.commit);
    });
  });

  // ---- initiatives and epics ----
  private hier = inject(Hierarchy);
  /** Initiative › epic › parent above this ticket, from the tickets on hand. */
  protected readonly crumbs = computed(() => this.hier.chain(this.id()));
  protected readonly container = computed(() => isContainer(this.detail()?.kind));
  private reloadTimer?: ReturnType<typeof setTimeout>;
  /** Sequence of load() and toggleCommit() calls: only the latest one lands. */
  private loadSeq = 0;
  private commitSeq = 0;
  /** Ka-Event-Seq of the last load: events up to it are already shown. */
  private loadedSeq = 0;

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
        return (p?.statuses ?? []).map((s) => ({ value: s.name, label: statusLabel(s.name), cat: s.category, hint: categoryLabel(s.category) }));
      case 'priority':
        return [...(sch?.priorities ?? ['P0', 'P1', 'P2', 'P3']).map((x) => ({ value: x, label: x, prio: x, hint: PRIO_HINT[x] })), { value: '', label: 'No priority' }];
      case 'kind':
        return [...(p?.kinds ?? []).map((k) => ({ value: k, label: kindLabel(k) })), { value: '', label: 'No kind' }];
      case 'assignee': {
        const you = this.live.you();
        const people = [you, ...(sch?.humans ?? []).filter((h) => h !== you), ...(sch?.agents ?? []), ...(sch?.roles ?? [])].filter(Boolean);
        return [{ value: '', label: 'Anyone' }, ...[...new Set(people)].map((a) => ({ value: a, label: actorName(a) + (a === you ? ' (you)' : ''), hint: a, actor: a.startsWith('role/') ? undefined : a }))];
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
      untracked(() => void this.load(id));
    });
    // Follow the ticket live: reload when an event touches it, unless the
    // last load already includes the event (the echo of our own write) or
    // it happened before this peek opened.
    const opened = Date.now();
    effect(() => {
      const t = this.live.touched();
      if (t && t.id === untracked(this.id) && t.at >= opened && t.seq > this.loadedSeq) {
        clearTimeout(this.reloadTimer);
        this.reloadTimer = setTimeout(() => void this.load(t.id), 200);
      }
    });
    inject(DestroyRef).onDestroy(() => clearTimeout(this.reloadTimer));
    // After a compaction every commit SHA changed: reload the timeline.
    let compactions = this.live.historyEpoch();
    effect(() => {
      const n = this.live.historyEpoch();
      if (n === compactions) return;
      compactions = n;
      untracked(() => {
        this.openCommit.set('');
        void this.load(this.id());
      });
    });
  }

  private async load(id: string): Promise<void> {
    const n = ++this.loadSeq;
    try {
      const [d, h] = await Promise.all([
        this.api.get<Detail>(`/tickets/${encodeURIComponent(id)}?include=comments`),
        this.api.get<{ events: KaEvent[] }>(`/activity?ticket=${encodeURIComponent(id)}&limit=300`),
      ]);
      if (n !== this.loadSeq || id !== this.id()) return;
      this.detail.set(d.body);
      this.etag = d.etag ?? '';
      this.history.set(h.body.events ?? []);
      this.loadedSeq = Math.min(d.seq, h.seq);
      this.error.set(null);
    } catch (e) {
      if (n !== this.loadSeq || id !== this.id()) return;
      const err = e instanceof ApiError ? e : new ApiError(0, { detail: String(e) });
      // A failed background reload keeps the ticket on screen; the next
      // event retries. Only a removed ticket replaces it with the error.
      if (this.detail() && err.status !== 404) return;
      this.error.set(err);
    }
  }

  protected async toggleCommit(e: Entry): Promise<void> {
    if (!e.commit) return;
    if (this.openCommit() === e.key) {
      this.openCommit.set('');
      return;
    }
    const n = ++this.commitSeq;
    this.openCommit.set(e.key);
    this.commit.set(null);
    this.commitError.set('');
    this.commitGone.set(false);
    try {
      const q = e.store ? `?store=${encodeURIComponent(e.store)}` : '';
      const r = await this.api.get<CommitView>(`/commits/${e.commit}${q}`);
      if (n === this.commitSeq) this.commit.set(r.body);
    } catch (err: any) {
      if (n !== this.commitSeq) return;
      // Commits from before a history compaction are simply gone.
      if (err instanceof ApiError && err.status === 404) this.commitGone.set(true);
      else this.commitError.set(err?.message ?? String(err));
    }
  }

  protected linkClick(e: MouseEvent): void {
    followInternalLink(e, this.router);
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
    // Escape (cancel) or Enter already ended the edit; the blur that follows
    // the input's removal must not save.
    if (!this.editingTitle()) return;
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
    setTimeout(() => this.host.nativeElement.querySelector<HTMLTextAreaElement>('.body-editor textarea')?.focus());
  }

  protected editTitle(): void {
    if (!this.writable()) return;
    this.editingTitle.set(true);
    setTimeout(() => {
      const el = this.host.nativeElement.querySelector<HTMLInputElement>('.title-edit');
      el?.focus();
      el?.select();
    });
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
    if (this.actions.dialogOpen() || isTyping(e) || document.querySelector('ka-palette, ka-shortcuts')) return;
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

function diffLines(diff: string): { cls: string; text: string }[] {
  return diff.split('\n').map((text) => ({
    text,
    cls: text.startsWith('+++') || text.startsWith('---') ? 'meta' : text.startsWith('+') ? 'add' : text.startsWith('-') ? 'del' : text.startsWith('@@') ? 'hunk' : text.startsWith('diff ') ? 'file' : '',
  }));
}

const PRIO_HINT: Record<string, string> = { P0: 'urgent', P1: 'high', P2: 'normal', P3: 'someday' };

function isTyping(e: KeyboardEvent): boolean {
  const t = e.target as HTMLElement | null;
  return !!t && (t.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(t.tagName));
}

const render = renderMarkdown;
