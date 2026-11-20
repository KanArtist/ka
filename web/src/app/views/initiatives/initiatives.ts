import { ChangeDetectionStrategy, Component, DestroyRef, computed, effect, inject, signal, untracked } from '@angular/core';
import { RouterLink } from '@angular/router';
import { Api } from '../../api/api';
import { Actions } from '../../actions';
import { Progress, TreeNode, fromRollup } from '../../hierarchy';
import { Live } from '../../live';
import { KindPipe, StatusPipe } from '../../ui/labels';
import { ProgressBar, progressText } from '../../ui/progress';
import { ProjectPipe } from '../../ui/project';

interface Row {
  node: TreeNode;
  progress: Progress;
  text: string;
  closed: boolean;
  /** Initiatives: their epics, one level down. */
  epics: Row[];
  /** Initiatives: direct children that are not epics. */
  direct: number;
}

/** The list refetches at most this often while tickets change. */
const REFRESH_MS = 1200;

/**
 * Initiatives (spec §4.5): every initiative and every epic without one, with
 * the engine's rollup over the work below it (GET /tree per project). One
 * click on a title filters the board to that initiative or epic.
 */
@Component({
  selector: 'ka-initiatives',
  imports: [RouterLink, ProgressBar, StatusPipe, KindPipe, ProjectPipe],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <section class="inits">
      <header class="head">
        <h1>Initiatives</h1>
        <div class="controls">
          @if (projects().length > 1) {
            <select [value]="project()" (change)="project.set($any($event.target).value)" aria-label="Project">
              <option value="" [selected]="!project()">All projects</option>
              @for (p of projects(); track p.key) {
                <option [value]="p.key" [selected]="p.key === project()">{{ p.name || p.key }}</option>
              }
            </select>
          }
          <label class="check"><input type="checkbox" [checked]="hideClosed()" (change)="hideClosed.set($any($event.target).checked)" /> Hide closed</label>
          @if (canCreate()) {
            <button type="button" class="btn quiet small" (click)="actions.fromLabel.set({ project: project() || undefined })">Convert a label…</button>
            <button type="button" class="btn small" (click)="actions.creating.set({ kind: 'initiative', project: project() || undefined })">New initiative</button>
          }
        </div>
      </header>
      <p class="muted lead">
        Long-running themes and the epics under them. Progress counts every work ticket below, at any depth. Click a title to see its work on the board; a container's
        status is yours to set.
      </p>
      @if (error()) {
        <p class="error-text">{{ error() }}</p>
      }

      @for (sec of sections(); track sec.title) {
        @if (sec.rows.length) {
          <h2>{{ sec.title }} <span class="n">{{ sec.rows.length }}</span></h2>
          <ul class="list">
            @for (r of sec.rows; track r.node.id) {
              <li class="item" [class.closed]="r.closed">
                <div class="line">
                  <span class="dot" [attr.data-cat]="r.node.category || null"></span>
                  <a class="title" routerLink="/board" [queryParams]="{ q: r.node.kind + ':' + r.node.id, board: null }" [title]="'Show ' + r.node.ref + ' on the board'">{{ r.node.title }}</a>
                  <span class="id">{{ r.node.id }}</span>
                  @if (projects().length > 1 && !project()) {
                    <span class="proj">{{ r.node.project | project }}</span>
                  }
                  <span class="spacer"></span>
                  <span class="status">{{ r.node.status | status }}</span>
                  <a class="btn quiet small" [routerLink]="[]" [queryParams]="{ peek: r.node.id }" queryParamsHandling="merge" [attr.aria-label]="'Open ' + r.node.id">Open</a>
                </div>
                <div class="meter">
                  <ka-progress class="wide" [value]="r.progress" />
                  <span class="text">{{ r.text }}{{ r.direct ? ' · ' + r.direct + ' directly below' : '' }}</span>
                </div>
                @if (r.epics.length) {
                  <ul class="epics" [attr.aria-label]="'Epics of ' + r.node.id">
                    @for (e of r.epics; track e.node.id) {
                      <li [class.closed]="e.closed">
                        <span class="dot" [attr.data-cat]="e.node.category || null"></span>
                        <span class="kind">{{ e.node.kind | kind }}</span>
                        <a class="etitle" routerLink="/board" [queryParams]="{ q: 'epic:' + e.node.id, board: null }" [title]="'Show ' + e.node.ref + ' on the board'">{{ e.node.title }}</a>
                        <a class="id" [routerLink]="[]" [queryParams]="{ peek: e.node.id }" queryParamsHandling="merge" [attr.aria-label]="'Open ' + e.node.id">{{ e.node.id }}</a>
                        <span class="spacer"></span>
                        <span class="status small">{{ e.node.status | status }}</span>
                        <ka-progress [value]="e.progress" />
                      </li>
                    }
                  </ul>
                }
              </li>
            }
          </ul>
        }
      }
      @if (loaded() && !total()) {
        <div class="empty">
          <p>No initiatives {{ hideClosed() ? 'open ' : '' }}yet.</p>
          <p class="muted">
            Create one, or turn a label you already use for a theme into one. Initiatives need every client on ka 1.3.0: the store must require it
            (<code>ka store require 1.3.0</code>).
          </p>
        </div>
      }
    </section>
  `,
  styles: `
    .inits {
      padding: 18px 24px 48px;
      max-width: 1000px;
    }
    .head {
      display: flex;
      align-items: center;
      flex-wrap: wrap;
      gap: 8px 16px;
    }
    h1 {
      margin: 0;
      font-size: var(--fs-xl);
      font-weight: 650;
      color: var(--text-strong);
    }
    .controls {
      display: flex;
      align-items: center;
      gap: 10px;
      margin-left: auto;
    }
    select {
      height: 26px;
      border: 1px solid var(--line);
      border-radius: var(--r-md);
      background: var(--surface);
      padding: 0 8px;
      font-size: var(--fs-sm);
    }
    .check {
      display: inline-flex;
      align-items: center;
      gap: 6px;
      font-size: var(--fs-sm);
      color: var(--muted);
    }
    .lead {
      max-width: 75ch;
      margin: 6px 0 4px;
    }
    h2 {
      margin: 22px 0 8px;
      font-size: var(--fs-lg);
      font-weight: 650;
      color: var(--text-strong);
    }
    .n {
      color: var(--faint);
      font-size: var(--fs-sm);
      font-weight: 500;
      margin-left: 4px;
    }
    .list {
      list-style: none;
      margin: 0;
      padding: 0;
      display: flex;
      flex-direction: column;
      gap: 8px;
    }
    .item {
      border: 1px solid var(--line-soft);
      border-radius: var(--r-lg);
      background: var(--surface);
      padding: 10px 12px;
    }
    .item.closed {
      opacity: 0.75;
    }
    .line,
    .epics li {
      display: flex;
      align-items: center;
      gap: 8px;
      min-width: 0;
    }
    .dot {
      width: 8px;
      height: 8px;
      border-radius: 50%;
      flex: none;
      background: var(--cat, var(--faint));
    }
    .title {
      font-family: var(--content);
      font-size: var(--fs-lg);
      font-weight: 600;
      color: var(--text-strong);
      text-decoration: none;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      min-width: 0;
    }
    .title:hover,
    .etitle:hover {
      text-decoration: underline;
      text-decoration-color: var(--faint);
      text-underline-offset: 3px;
    }
    .proj,
    .status {
      font-size: var(--fs-sm);
      color: var(--muted);
      white-space: nowrap;
    }
    .spacer {
      flex: 1;
    }
    .meter {
      display: flex;
      align-items: center;
      gap: 12px;
      margin: 8px 0 0 16px;
      font-size: var(--fs-sm);
      color: var(--muted);
    }
    ka-progress.wide {
      --bar-w: 220px;
    }
    .epics {
      list-style: none;
      margin: 10px 0 0 16px;
      padding: 6px 0 0;
      border-top: 1px solid var(--line-soft);
    }
    .epics li {
      line-height: 26px;
    }
    .kind {
      font-size: var(--fs-xs);
      font-weight: 600;
      color: var(--muted);
      border: 1px solid var(--line);
      border-radius: var(--r-sm);
      padding: 0 4px;
      line-height: 15px;
    }
    .etitle {
      font-family: var(--content);
      color: var(--text-strong);
      text-decoration: none;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      min-width: 0;
    }
    a.id,
    a.btn {
      text-decoration: none;
    }
    .small {
      font-size: var(--fs-xs);
    }
    .empty {
      margin-top: 28px;
      max-width: 70ch;
    }
  `,
})
export class Initiatives {
  protected live = inject(Live);
  protected actions = inject(Actions);
  private api = inject(Api);

  protected readonly projects = computed(() => (this.live.schema()?.projects ?? []).filter((p) => !p.archived));
  protected readonly project = signal('');
  protected readonly hideClosed = signal(true);
  protected readonly nodes = signal<TreeNode[]>([]);
  protected readonly loaded = signal(false);
  protected readonly error = signal('');
  protected readonly canCreate = computed(() => this.projects().some((p) => !p.read_only && (p.kinds ?? []).includes('initiative')));

  private readonly rows = computed(() => {
    const hide = this.hideClosed();
    const project = this.project();
    const isClosed = (n: TreeNode) => n.category === 'completed' || n.category === 'canceled';
    const row = (n: TreeNode): Row => {
      const progress = fromRollup(n.rollup) ?? { total: 0, done: 0, canceled: 0, in_progress: 0, blocked: 0, ready: 0 };
      const kids = n.children ?? [];
      const epics = n.kind === 'initiative' ? kids.filter((c) => c.kind === 'epic' && !(hide && isClosed(c))).map(row) : [];
      return { node: n, progress, text: progressText(progress), closed: isClosed(n), epics, direct: n.kind === 'initiative' ? kids.filter((c) => c.kind !== 'epic').length + (n.more ?? 0) : 0 };
    };
    return this.nodes()
      .filter((n) => (!project || n.project === project) && !(hide && isClosed(n)))
      .map(row);
  });

  protected readonly sections = computed(() => {
    const rows = this.rows();
    return [
      { title: 'Initiatives', rows: rows.filter((r) => r.node.kind === 'initiative') },
      { title: 'Epics without an initiative', rows: rows.filter((r) => r.node.kind === 'epic') },
    ];
  });
  protected readonly total = computed(() => this.rows().length);

  private seq = 0;

  constructor() {
    this.actions.showsProject(() => this.project());
    // Rollups change with any ticket below; refetch once changes settle,
    // never per event.
    let t: ReturnType<typeof setTimeout> | undefined;
    inject(DestroyRef).onDestroy(() => clearTimeout(t));
    let first = true;
    effect(() => {
      this.live.ticketTick();
      this.projects();
      untracked(() => {
        if (first) {
          first = false;
          void this.load();
        } else if (!t) {
          // At most one refetch per REFRESH_MS, however busy the stream is.
          t = setTimeout(() => {
            t = undefined;
            void this.load();
          }, REFRESH_MS);
        }
      });
    });
  }

  /** GET /tree without an id, once per project: its initiatives and top-level epics, one level deep. */
  private async load(): Promise<void> {
    const n = ++this.seq;
    try {
      const keys = this.projects().map((p) => p.key);
      const results = await Promise.all(keys.map((k) => this.api.get<{ nodes: TreeNode[] | null }>(`/tree?project=${encodeURIComponent(k)}&depth=1`)));
      if (n !== this.seq) return;
      this.nodes.set(results.flatMap((r) => r.body.nodes ?? []));
      this.error.set('');
    } catch (e: any) {
      if (n !== this.seq) return;
      this.error.set(e?.message ?? String(e));
    }
    this.loaded.set(true);
  }
}
