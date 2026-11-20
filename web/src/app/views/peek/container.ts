import { ChangeDetectionStrategy, Component, DestroyRef, computed, effect, inject, input, signal, untracked } from '@angular/core';
import { Router, RouterLink } from '@angular/router';
import { Api, Detail } from '../../api/api';
import { Actions } from '../../actions';
import { Hierarchy, Progress, TreeNode, fromRollup } from '../../hierarchy';
import { Live } from '../../live';
import { ActorPipe, Avatar } from '../../ui/actor';
import { KindPipe, StatusPipe } from '../../ui/labels';
import { ProgressBar } from '../../ui/progress';

/** One visible row of the tree. */
interface TreeRow {
  node: TreeNode;
  depth: number;
  kids: number;
  open: boolean;
  progress?: Progress;
  /** A "… N more" row for children the tree left out. */
  more?: number;
}

/** How long the tree waits after a change below it before refetching. */
const TREE_DEBOUNCE_MS = 700;

/**
 * An initiative's or epic's page (spec §4.5): the engine's rollup over every
 * work ticket below it, as a progress bar with its counts, and the tree of
 * children from GET /tree?id=, collapsible, each row opening its ticket. It
 * refetches, debounced, when an event touches any ticket below it, so a
 * burst of agent activity costs one request.
 */
@Component({
  selector: 'ka-container',
  imports: [RouterLink, Avatar, ActorPipe, KindPipe, StatusPipe, ProgressBar],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (ticket(); as t) {
      <section class="rollup" [attr.aria-label]="(t.kind | kind) + ' progress'">
        @if (rollup(); as r) {
          <ka-progress class="block" [value]="r" [compact]="false" />
        } @else if (treeError()) {
          <p class="error-text">Could not load the progress: {{ treeError() }}</p>
        } @else {
          <p class="muted">Counting the work below…</p>
        }
        <div class="rollup-actions">
          <button type="button" class="btn small" (click)="showOnBoard()" title="The board, filtered to {{ t.kind }}:{{ t.id }}">Show on board</button>
          @if (writable()) {
            @if (t.kind === 'initiative' && (kinds()).includes('epic')) {
              <button type="button" class="btn quiet small" (click)="addChild('epic')">Add epic</button>
            }
            <button type="button" class="btn quiet small" (click)="addChild()">Add ticket</button>
          }
          <span class="spacer"></span>
          <span class="muted small-note">Its status is set by hand; nothing closes it for you.</span>
        </div>
      </section>
      <section class="tree-sec">
        <div class="tree-head">
          <h2 [id]="'below-' + t.id">
            Below this {{ t.kind }}
            @if (rollup(); as r) {
              <span class="count">{{ r.total }} work {{ r.total === 1 ? 'ticket' : 'tickets' }}</span>
            }
          </h2>
          <span class="spacer"></span>
          <label class="hide-closed"><input type="checkbox" [checked]="hideClosed()" (change)="toggleHideClosed()" /> Hide closed</label>
        </div>
        <ul class="tree" [attr.aria-labelledby]="'below-' + t.id">
          @for (r of treeRows(); track (r.more ? 'more:' : '') + r.node.id) {
            @if (r.more) {
              <li class="trow more" [style.--depth]="r.depth">… {{ r.more }} more not shown</li>
            } @else {
              <li class="trow" [style.--depth]="r.depth" [class.closed]="r.node.category === 'completed' || r.node.category === 'canceled'">
                @if (r.kids) {
                  <button type="button" class="fold" [attr.aria-expanded]="r.open" [attr.aria-label]="(r.open ? 'Collapse ' : 'Expand ') + r.node.id" (click)="fold(r.node.id)">
                    <span class="chev" aria-hidden="true"></span>
                  </button>
                } @else {
                  <span class="fold" aria-hidden="true"></span>
                }
                <a class="tlink" [routerLink]="[]" [queryParams]="{ peek: r.node.id }" queryParamsHandling="merge" [title]="r.node.ref">
                  <span class="dot" [attr.data-cat]="r.node.category || null" [title]="r.node.status | status"></span>
                  @if (r.node.kind === 'initiative' || r.node.kind === 'epic') {
                    <span class="ckind">{{ r.node.kind | kind }}</span>
                  }
                  <span class="id">{{ r.node.id }}</span>
                  <span class="ttitle">{{ r.node.title }}</span>
                  <span class="sr-only">, {{ r.node.status | status }}</span>
                </a>
                @if (r.node.blocked && r.node.status !== 'blocked') {
                  <span class="flag blocked">blocked</span>
                }
                @if (r.progress) {
                  <ka-progress [value]="r.progress" />
                }
                @if (r.node.priority) {
                  <span class="prio" [attr.data-p]="r.node.priority">{{ r.node.priority }}</span>
                }
                @if (r.node.claimed_by) {
                  <span class="claimer" [title]="'Claimed by ' + r.node.claimed_by"><ka-avatar [actor]="r.node.claimed_by" [size]="16" /><span class="sr-only">claimed by {{ r.node.claimed_by | actor }}</span></span>
                }
              </li>
            }
          } @empty {
            @if (tree()) {
              <li class="muted">Nothing below it yet.</li>
            }
          }
        </ul>
      </section>
    }
  `,
  styles: `
    :host {
      display: block;
      margin-bottom: 26px;
    }
    section {
      margin-top: 26px;
    }
    h2 {
      margin: 0 0 8px;
      font-size: var(--fs-md);
      font-weight: 650;
      color: var(--text-strong);
    }
    h2 .count {
      color: var(--muted);
      font-weight: 500;
      margin-left: 4px;
    }
    .spacer {
      flex: 1;
    }
    .dot {
      width: 8px;
      height: 8px;
      border-radius: 50%;
      flex: none;
      display: inline-block;
      background: var(--cat, var(--faint));
    }
    .flag {
      font-size: var(--fs-xs);
      font-weight: 600;
      padding: 1px 6px;
      border-radius: var(--r-sm);
    }
    .flag.blocked {
      color: var(--st-blocked);
      background: color-mix(in srgb, var(--st-blocked) 14%, transparent);
    }
    .prio[data-p='P0'] {
      color: var(--prio-0) !important;
    }
    .prio[data-p='P1'] {
      color: var(--prio-1) !important;
    }
    .ckind {
      font-size: var(--fs-xs);
      font-weight: 600;
      color: var(--muted);
      border: 1px solid var(--line);
      border-radius: var(--r-sm);
      padding: 0 4px;
      line-height: 15px;
      flex: none;
    }
    .ttitle {
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      font-family: var(--content);
    }
    .rollup {
      margin-top: 0;
      margin-bottom: 18px;
      padding: 12px 14px;
      border: 1px solid var(--line-soft);
      border-radius: var(--r-lg);
      background: var(--surface);
      max-width: 72ch;
    }
    .rollup-actions {
      display: flex;
      align-items: center;
      flex-wrap: wrap;
      gap: 6px;
      margin-top: 10px;
    }
    .small-note {
      font-size: var(--fs-xs);
    }
    .tree-head {
      display: flex;
      align-items: baseline;
      gap: 4px;
      margin-bottom: 8px;
    }
    .tree-head h2 {
      margin: 0;
    }
    .hide-closed {
      display: inline-flex;
      align-items: center;
      gap: 5px;
      font-size: var(--fs-sm);
      font-weight: 400;
      color: var(--muted);
    }
    .tree {
      list-style: none;
      margin: 0;
      padding: 0;
    }
    .trow {
      --depth: 0;
      display: flex;
      align-items: center;
      gap: 8px;
      min-height: 28px;
      padding-left: calc(var(--depth) * 18px);
      border-radius: var(--r-md);
    }
    .trow:hover {
      background: var(--row-hover);
    }
    .trow.more {
      padding-left: calc(var(--depth) * 18px + 26px);
      color: var(--faint);
      font-size: var(--fs-sm);
    }
    .trow.closed .ttitle {
      color: var(--muted);
    }
    .fold {
      width: 18px;
      height: 18px;
      flex: none;
      display: grid;
      place-items: center;
      border: 0;
      padding: 0;
      background: none;
      border-radius: var(--r-sm);
    }
    button.fold:hover {
      background: var(--raised);
    }
    .fold .chev {
      width: 6px;
      height: 6px;
      border-right: 1.5px solid var(--muted);
      border-bottom: 1.5px solid var(--muted);
      transform: rotate(-45deg);
      transition: transform 120ms var(--ease);
    }
    .fold[aria-expanded='true'] .chev {
      transform: rotate(45deg);
    }
    .tlink {
      flex: 1;
      min-width: 0;
      display: flex;
      align-items: center;
      gap: 7px;
      text-decoration: none;
      color: var(--text-strong);
    }
    .tlink:hover .ttitle {
      text-decoration: underline;
      text-decoration-color: var(--faint);
      text-underline-offset: 3px;
    }
    .trow .prio {
      font-size: var(--fs-xs);
      font-weight: 650;
      color: var(--muted);
    }
    .claimer {
      display: inline-flex;
    }
  `,
})
export class ContainerPanel {
  /** The container, as the peek loaded it; a new value (a reload) refetches the tree. */
  readonly ticket = input.required<Detail>();
  readonly writable = input(false);
  /** The project's kinds, to offer "Add epic" only where epics exist. */
  readonly kinds = input<readonly string[]>([]);

  private api = inject(Api);
  private live = inject(Live);
  private hier = inject(Hierarchy);
  private actions = inject(Actions);
  private router = inject(Router);

  /** The subtree; its root is this container. */
  protected readonly tree = signal<TreeNode | null>(null);
  protected readonly treeError = signal('');
  protected readonly hideClosed = signal(false);
  /** Nodes the reader collapsed. */
  protected readonly folded = signal<ReadonlySet<string>>(new Set());
  protected readonly rollup = computed(() => fromRollup(this.tree()?.rollup));

  /** IDs in the loaded tree, so a change to any of them refetches it. */
  private readonly treeIds = computed(() => {
    const ids = new Set<string>();
    const walk = (n: TreeNode) => {
      ids.add(n.id);
      for (const c of n.children ?? []) walk(c);
    };
    const t = this.tree();
    if (t) walk(t);
    return ids;
  });

  protected readonly treeRows = computed<TreeRow[]>(() => {
    const root = this.tree();
    const folded = this.folded();
    const out: TreeRow[] = [];
    const walk = (n: TreeNode, depth: number) => {
      for (const c of n.children ?? []) {
        const open = !folded.has(c.id);
        out.push({ node: c, depth, kids: (c.children?.length ?? 0) + (c.more ?? 0), open, progress: fromRollup(c.rollup) });
        if (open) walk(c, depth + 1);
      }
      if (n.more) out.push({ node: n, depth, kids: 0, open: false, more: n.more });
    };
    if (root) walk(root, 0);
    return out;
  });

  private seq = 0;
  private timer?: ReturnType<typeof setTimeout>;

  constructor() {
    // The container itself changed (the peek reloaded it): refetch now.
    effect(() => {
      this.ticket();
      untracked(() => void this.load());
    });
    // A change to a ticket in the tree, or to one now below this container
    // (a new child, one moved in), refetches once the changes settle.
    const opened = Date.now();
    let handled = 0;
    effect(() => {
      const t = this.live.touched();
      if (!t || t.seq === handled || t.at < opened) return;
      handled = t.seq;
      untracked(() => {
        const id = this.ticket().id;
        if (t.id !== id && (this.treeIds().has(t.id) || this.hier.isBelow(t.id, id))) this.schedule();
      });
    });
    inject(DestroyRef).onDestroy(() => clearTimeout(this.timer));
  }

  private schedule(): void {
    clearTimeout(this.timer);
    this.timer = setTimeout(() => void this.load(), TREE_DEBOUNCE_MS);
  }

  private async load(): Promise<void> {
    clearTimeout(this.timer);
    const id = this.ticket().id;
    const n = ++this.seq;
    try {
      const r = await this.api.get<{ nodes: TreeNode[] | null }>(`/tree?id=${encodeURIComponent(id)}${this.hideClosed() ? '&hide_closed=true' : ''}`);
      if (n !== this.seq) return;
      this.tree.set(r.body.nodes?.[0] ?? null);
      this.treeError.set('');
    } catch (e: any) {
      if (n !== this.seq) return;
      this.treeError.set(e?.message ?? String(e));
    }
  }

  protected toggleHideClosed(): void {
    this.hideClosed.update((v) => !v);
    void this.load();
  }

  protected fold(id: string): void {
    this.folded.update((s) => {
      const n = new Set(s);
      if (n.has(id)) n.delete(id);
      else n.add(id);
      return n;
    });
  }

  /** A new ticket under this container: an epic, or work. */
  protected addChild(kind?: string): void {
    const t = this.ticket();
    this.actions.creating.set({ project: t.project, parent: t.id, kind });
  }

  /** The board, filtered to everything below this container. */
  protected showOnBoard(): void {
    const t = this.ticket();
    if (!t.kind) return;
    void this.router.navigate(['/board'], { queryParams: { q: `${t.kind}:${t.id}`, board: null, peek: null } });
  }
}
