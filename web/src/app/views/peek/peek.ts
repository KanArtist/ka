import { ChangeDetectionStrategy, Component, computed, effect, inject, input, output, signal, untracked } from '@angular/core';
import { Router, RouterLink } from '@angular/router';
import { marked } from 'marked';
import { Api, ApiError, CommentOut, Detail, KaEvent } from '../../api/api';
import { Live } from '../../live';
import { Actor, isAgent } from '../../ui/actor';
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
  imports: [RouterLink, Actor],
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

  protected readonly detail = signal<Detail | null>(null);
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

  /** A related ticket's status category, when it is in the workspace. */
  protected relCat(id: string): string {
    const t = this.live.tickets().get(id);
    return t ? this.live.category(t) : '';
  }
}

/** Markdown to HTML. Angular's [innerHTML] sanitizer strips scripts, handlers and unsafe URLs. */
function render(md: string | undefined): string {
  if (!md?.trim()) return '';
  return marked.parse(md, { gfm: true, async: false }) as string;
}
