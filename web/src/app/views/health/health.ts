import { ChangeDetectionStrategy, Component, DestroyRef, ElementRef, computed, effect, inject, signal, untracked, viewChild } from '@angular/core';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { NavigationEnd, Router, RouterLink } from '@angular/router';
import { Api, ApiError, InboxItem, StoreOut } from '../../api/api';
import type { components } from '../../api/schema';
import { Actions } from '../../actions';
import { Live } from '../../live';
import { ActorPipe } from '../../ui/actor';
import { WordsPipe } from '../../ui/labels';
import { followInternalLink, renderMarkdown } from '../../ui/markdown';
import { Toasts } from '../../ui/toast';
import { Clock, ago, exact } from '../../ui/time';

interface Diagnostic {
  code: string;
  severity: 'error' | 'warning' | 'info';
  path: string;
  line?: number;
  message: string;
  fix?: string;
}

type HistoryOut = components['schemas']['HistoryOut'];
type HistoryEntry = components['schemas']['HistoryEntry'];
type CompactOut = components['schemas']['CompactOut'];

/** A compaction in progress in the dialog. */
interface Compacting {
  store: string;
  plan?: CompactOut;
  /** The dry run or the real run failed (git_busy, a refused force push, not level with upstream, …). */
  problem?: { message: string; fix?: string; status?: number };
  running?: boolean;
  done?: CompactOut;
}

const STATE_TEXT: Record<string, string> = {
  synced: 'In sync',
  local: 'Local only (no remote)',
  ahead: 'Commits waiting to push',
  behind: 'Behind the remote',
  diverged: 'Diverged; the next sync replays local commits',
  degraded: 'Degraded: working locally',
  busy: 'Busy',
};

/**
 * Sync & health (PRD §13.2): each store's sync state, last fetch and push,
 * degraded mode, conflicts preserved during syncs (with restore), and
 * ka check findings.
 */
@Component({
  selector: 'ka-health',
  imports: [RouterLink, WordsPipe, ActorPipe],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <section class="health">
      <header class="head">
        <h1>Sync &amp; health</h1>
        <button type="button" class="btn small" (click)="syncNow()" [disabled]="syncing()">{{ syncing() ? 'Syncing…' : 'Sync now' }}</button>
      </header>

      <h2>Stores</h2>
      <div class="stores">
        @for (s of live.stores(); track s.store) {
          <article class="store" [attr.data-state]="s.state" [attr.id]="'store-' + s.store">
            <header>
              <span class="name">{{ s.store }}</span>
              @if (s.read_only) {
                <span class="chip">read-only</span>
              }
              <span class="spacer"></span>
              <span class="state"><span class="pip"></span>{{ stateText(s) }}</span>
            </header>
            @if (s.detail) {
              <p class="detail">{{ s.detail }}</p>
            }
            <dl>
              <dt>Remote</dt>
              <dd class="mono">{{ s.url || s.remote || 'none' }}{{ s.branch ? ' · ' + s.branch : '' }}</dd>
              <dt>Ahead / behind</dt>
              <dd>{{ s.ahead }} / {{ s.behind }}{{ s.push_pending ? ' · push scheduled' : '' }}</dd>
              <dt>Push</dt>
              <dd>{{ s.push_mode | words }}</dd>
              <dt>Fetches</dt>
              <dd>every {{ s.fetch_interval }}</dd>
              <dt>Last fetch</dt>
              <dd [title]="exact(s.last_fetch ?? '')">{{ s.last_fetch ? ago(s.last_fetch, now()) : 'not yet' }}</dd>
              <dt>Last push</dt>
              <dd [title]="exact(s.last_push ?? '')">{{ s.last_push ? ago(s.last_push, now()) : 'not yet' }}</dd>
              <dt>Head</dt>
              <dd class="mono">{{ s.head }}</dd>
              <dt>Checkout</dt>
              <dd class="mono path" [title]="s.root">{{ s.managed ? 'managed clone' : 'attached' }} · {{ s.root }}</dd>
            </dl>
            <section class="history" [attr.aria-label]="'History of ' + s.store">
              @if (histories().get(s.store); as h) {
                <p class="hstate">
                  @if (h.compacted) {
                    <span class="pill">compacted</span> Its git history starts at <span class="mono">{{ (h.root ?? '').slice(0, 10) }}</span>; what came before is summarized below.
                  } @else if (h.summaries?.length) {
                    History summaries from an earlier compaction:
                  } @else {
                    Full history: never compacted.
                  }
                </p>
                @if (h.summaries?.length) {
                  <ul class="summaries">
                    @for (e of h.summaries; track e.name) {
                      <li [class.open]="openSummary()?.store === s.store && openSummary()?.name === e.name" [attr.id]="'sum-' + s.store + '-' + e.name">
                        <button type="button" class="sumbtn" [attr.aria-expanded]="openSummary()?.store === s.store && openSummary()?.name === e.name" (click)="toggleSummary(s.store, e.name)">
                          <span class="when" [title]="exact(e.compacted_at)">{{ day(e.compacted_at) }}</span>
                          <span class="facts">
                            {{ e.commits }} commits{{ e.first_commit ? ', ' + day(e.first_commit) + ' – ' + day(e.last_commit) : '' }}
                            @if (e.tickets_created || e.tickets_closed) {
                              · {{ e.tickets_created ?? 0 }} created, {{ e.tickets_closed ?? 0 }} closed
                            }
                          </span>
                          <span class="by">by {{ e.by | actor }}</span>
                        </button>
                        @if (openSummary()?.store === s.store && openSummary()?.name === e.name) {
                          <div class="sumbody">
                            @if (summary(); as full) {
                              <dl class="sumfacts">
                                <dt>File</dt>
                                <dd class="mono">{{ full.path }}</dd>
                                <dt>Compacted</dt>
                                <dd>{{ exact(full.compacted_at) }} by {{ full.by | actor }}{{ full.on_behalf_of ? ' for ' + (full.on_behalf_of | actor) : '' }}</dd>
                                <dt>Dropped</dt>
                                <dd>{{ full.commits }} commits, {{ exact(full.first_commit) || '?' }} to {{ exact(full.last_commit) || '?' }}</dd>
                                <dt>Last commit</dt>
                                <dd class="mono">{{ full.from }}</dd>
                                @if (actorsOf(full).length) {
                                  <dt>Commits by</dt>
                                  <dd>
                                    @for (a of actorsOf(full); track a[0]; let last = $last) {
                                      {{ a[0] | actor }} {{ a[1] }}{{ last ? '' : ', ' }}
                                    }
                                  </dd>
                                }
                                @if (full.previous) {
                                  <dt>Previous</dt>
                                  <dd><button type="button" class="linkish mono" (click)="toggleSummary(s.store, full.previous)">{{ full.previous }}</button></dd>
                                }
                              </dl>
                              @if (summaryHtml()) {
                                <div class="md" (click)="linkClick($event)" [innerHTML]="summaryHtml()"></div>
                              } @else {
                                <p class="muted">No narrative was written for this compaction.</p>
                              }
                            } @else if (summaryError()) {
                              <p class="error-text">{{ summaryError() }}</p>
                            } @else {
                              <p class="muted">Loading the summary…</p>
                            }
                          </div>
                        }
                      </li>
                    }
                  </ul>
                }
              } @else if (historyErrors().get(s.store); as err) {
                <p class="hstate muted">History summaries unavailable: {{ err }}</p>
              }
              @if (!s.read_only) {
                <div class="hactions">
                  @if (forbidden().has(s.store)) {
                    <button type="button" class="btn small" disabled aria-describedby="forbid-note">Compact history…</button>
                    <span id="forbid-note" class="muted small">Only the daemon's own machine can compact a store: run <code>ka compact --store {{ s.store }}</code> there.</span>
                  } @else {
                    <button #compactBtn type="button" class="btn small" (click)="startCompact(s.store, compactBtn)" [disabled]="!s.remote">Compact history…</button>
                    @if (!s.remote) {
                      <span class="muted small">Needs a remote: a local store has nothing to share.</span>
                    }
                  }
                </div>
              }
            </section>
          </article>
        }
      </div>

      <h2>Preserved conflicts <span class="n">{{ conflicts().length }}</span></h2>
      <p class="muted lead">When a sync cannot apply someone's change, ka keeps it as a comment on the ticket instead of dropping it. Operations can be re-applied here.</p>
      @for (c of conflicts(); track c.id + c.comment.name) {
        <div class="conflict">
          <a class="ticket" [routerLink]="[]" [queryParams]="{ peek: c.id }" queryParamsHandling="merge"><span class="id">{{ c.id }}</span> {{ c.title }}</a>
          <p class="body">{{ c.comment.body }}</p>
          <div class="row">
            <span class="muted" [title]="exact(c.comment.created)">{{ ago(c.comment.created, now()) }}</span>
            <span class="spacer"></span>
            @if (c.restorable) {
              <button type="button" class="btn small" (click)="restore(c)" [disabled]="restoring() === c.comment.name">Restore</button>
            } @else {
              <span class="muted small">A conflicting value; open the ticket to copy it back.</span>
            }
          </div>
        </div>
      } @empty {
        <p class="muted">None in the last week.</p>
      }

      <h2>
        ka check
        <span class="n">{{ counts().error }} errors, {{ counts().warning }} warnings, {{ counts().info }} info</span>
      </h2>
      @if (checkError()) {
        <p class="error-text">{{ checkError() }}</p>
      }
      <ul class="diags">
        @for (d of shownDiags(); track $index) {
          <li [attr.data-sev]="d.severity">
            <span class="sev">{{ d.severity }}</span>
            <span class="code" [title]="d.code">{{ d.code | words }}</span>
            <span class="msg">{{ d.message }}</span>
            <span class="where mono">{{ d.path }}{{ d.line ? ':' + d.line : '' }}</span>
            @if (d.fix) {
              <span class="fix">{{ d.fix }}</span>
            }
          </li>
        } @empty {
          @if (checked()) {
            <li class="clean">Every store passes ka check.</li>
          }
        }
      </ul>
      @if (counts().info && !showInfo()) {
        <button type="button" class="btn quiet small" (click)="showInfo.set(true)">Show {{ counts().info }} info findings</button>
      }
    </section>

    @if (compacting(); as c) {
      <div class="scrim" (click)="closeCompact()">
        <form
          #compactForm
          class="dialog"
          role="dialog"
          aria-modal="true"
          aria-labelledby="compact-title"
          tabindex="-1"
          (click)="$event.stopPropagation()"
          (keydown)="compactKey($event)"
          (submit)="$event.preventDefault(); runCompact()"
        >
          <h2 id="compact-title">Compact the history of {{ c.store }}</h2>
          @if (c.done; as d) {
            <p>
              Done. {{ d.commits }} commits became one root commit (<span class="mono">{{ (d.root ?? '').slice(0, 10) }}</span>), force-pushed to {{ d.remote }}/{{ d.branch }}.
              The summary is <span class="mono">{{ d.summary_file }}</span>.
            </p>
            <p class="muted">Object store: {{ bytes(d.size_before) }} before, {{ d.size_after ? bytes(d.size_after) : 'unknown' }} after.</p>
            @if (d.warnings?.length) {
              <ul class="warns">
                @for (w of d.warnings; track $index) {
                  <li>{{ w }}</li>
                }
              </ul>
            }
            <div class="dactions">
              <span class="spacer"></span>
              <button type="button" class="btn primary" (click)="closeCompact()">Close</button>
            </div>
          } @else if (c.plan; as p) {
            <p class="danger" role="note">
              <strong>This rewrites the shared history and cannot be undone on the remote.</strong>
              The data stays exactly as it is, but every commit before now is replaced by one root commit, force-pushed to {{ p.remote }}/{{ p.branch }}. Every clone
              moves to the new history on its next sync; links to old commits stop resolving.
            </p>
            <dl class="plan">
              <dt>Commits dropped</dt>
              <dd>{{ p.commits }}{{ p.first_commit ? ', ' + exact(p.first_commit) + ' to ' + exact(p.last_commit) : '' }}</dd>
              @if (actorsOf(p).length) {
                <dt>By</dt>
                <dd>
                  @for (a of actorsOf(p); track a[0]; let last = $last) {
                    {{ a[0] | actor }} {{ a[1] }}{{ last ? '' : ', ' }}
                  }
                </dd>
              }
              @if (p.tickets_created || p.tickets_closed) {
                <dt>Tickets</dt>
                <dd>{{ p.tickets_created ?? 0 }} created, {{ p.tickets_closed ?? 0 }} closed in that history</dd>
              }
              <dt>Size now</dt>
              <dd>{{ bytes(p.size_before) }} of git objects in this clone</dd>
              <dt>Summary file</dt>
              <dd><span class="mono">{{ p.summary_file }}</span> <span class="muted">(a sample name; the real one carries the time it runs)</span></dd>
              @if (p.previous) {
                <dt>Previous</dt>
                <dd class="mono">{{ p.previous }}</dd>
              }
              <dt>requires_ka</dt>
              <dd>
                @if (p.raises_requires_ka) {
                  Raised from {{ p.requires_ka_was || 'none' }} to <strong>{{ p.requires_ka }}</strong>: clients older than that must upgrade before they can use the store.
                } @else {
                  Stays {{ p.requires_ka }}
                }
              </dd>
              <dt>Backups</dt>
              <dd>{{ p.backup_refs ? p.backup_refs + ' replay backup ref(s) (refs/ka/backup/*) are deleted with the old history' : 'No replay backups to delete' }}</dd>
            </dl>
            @if (p.warnings?.length) {
              <ul class="warns" aria-label="Warnings">
                @for (w of p.warnings; track $index) {
                  <li>{{ w }}</li>
                }
              </ul>
            }
            <label>
              Narrative (optional, Markdown): what this history held, for whoever reads the summary later
              <textarea #narrative class="field" rows="4" [value]="narrativeText()" (input)="narrativeText.set(narrative.value)" [disabled]="!!c.running"></textarea>
            </label>
            <label>
              <span>Type <strong class="confirm-name">{{ c.store }}</strong> to confirm</span>
              <input #confirmIn class="field mono" type="text" autocomplete="off" spellcheck="false" [value]="confirmText()" (input)="confirmText.set(confirmIn.value)" [disabled]="!!c.running" />
            </label>
            @if (c.problem; as pr) {
              <div class="problem" role="alert">
                <p class="error-text">{{ pr.message }}</p>
                @if (pr.fix) {
                  <p class="muted">{{ pr.fix }}</p>
                }
              </div>
            }
            <div class="dactions">
              <span class="muted small spacer">Under the store's write lock; agents' writes wait meanwhile.</span>
              <button type="button" class="btn quiet" (click)="closeCompact()" [disabled]="!!c.running">Cancel</button>
              <button type="submit" class="btn primary danger-btn" [disabled]="confirmText() !== c.store || !!c.running">{{ c.running ? 'Compacting…' : 'Compact history' }}</button>
            </div>
          } @else if (c.problem; as pr) {
            <div class="problem" role="alert">
              <p class="error-text">{{ pr.message }}</p>
              @if (pr.fix) {
                <p class="muted">{{ pr.fix }}</p>
              }
            </div>
            <div class="dactions">
              <span class="spacer"></span>
              <button type="button" class="btn" (click)="closeCompact()">Close</button>
            </div>
          } @else {
            <p class="muted" role="status">Checking what a compaction would do (a dry run; nothing changes)…</p>
          }
        </form>
      </div>
    }
  `,
  styles: `
    .health {
      padding: 18px 24px 48px;
      max-width: 1100px;
    }
    .head {
      display: flex;
      align-items: center;
      gap: 12px;
    }
    h1 {
      margin: 0;
      font-size: var(--fs-xl);
      font-weight: 650;
      color: var(--text-strong);
    }
    h2 {
      margin: 26px 0 8px;
      font-size: var(--fs-lg);
      font-weight: 650;
      color: var(--text-strong);
    }
    h2 .n {
      color: var(--faint);
      font-size: var(--fs-sm);
      font-weight: 500;
      margin-left: 6px;
    }
    .lead {
      margin: -2px 0 10px;
      max-width: 70ch;
    }
    .stores {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(340px, 1fr));
      gap: 10px;
    }
    .store {
      border: 1px solid var(--line-soft);
      border-radius: var(--r-lg);
      background: var(--surface);
      padding: 10px 12px;
      --pip: var(--st-completed);
    }
    .store[data-state='degraded'],
    .store[data-state='diverged'] {
      --pip: var(--st-blocked);
      border-color: color-mix(in srgb, var(--st-blocked) 45%, var(--line-soft));
    }
    .store[data-state='ahead'],
    .store[data-state='behind'],
    .store[data-state='busy'] {
      --pip: var(--ink-human);
    }
    .store[data-state='local'] {
      --pip: var(--faint);
    }
    .store header {
      display: flex;
      align-items: center;
      gap: 8px;
    }
    .name {
      font-weight: 650;
      color: var(--text-strong);
    }
    .chip {
      font-size: var(--fs-xs);
      color: var(--muted);
      border: 1px solid var(--line);
      border-radius: 9px;
      padding: 0 6px;
    }
    .spacer {
      flex: 1;
    }
    .state {
      display: inline-flex;
      align-items: center;
      gap: 6px;
      font-size: var(--fs-sm);
      color: var(--text);
    }
    .pip {
      width: 8px;
      height: 8px;
      border-radius: 50%;
      background: var(--pip);
    }
    .detail {
      margin: 6px 0 0;
      font-size: var(--fs-sm);
      color: var(--st-blocked);
      overflow-wrap: anywhere;
    }
    dl {
      display: grid;
      grid-template-columns: 110px 1fr;
      gap: 2px 10px;
      margin: 8px 0 0;
      font-size: var(--fs-sm);
    }
    dt {
      color: var(--muted);
    }
    dd {
      margin: 0;
      min-width: 0;
      color: var(--text);
    }
    .mono {
      font-family: var(--mono);
      font-size: var(--fs-xs);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    .conflict {
      border: 1px solid var(--line-soft);
      border-radius: var(--r-md);
      padding: 8px 10px;
      margin-bottom: 8px;
      max-width: 80ch;
    }
    .ticket {
      text-decoration: none;
      color: var(--text-strong);
    }
    .body {
      margin: 4px 0;
      color: var(--text);
      font-size: var(--fs-sm);
      white-space: pre-wrap;
    }
    .row {
      display: flex;
      align-items: center;
      gap: 8px;
      font-size: var(--fs-sm);
    }
    .small {
      font-size: var(--fs-sm);
    }
    .diags {
      list-style: none;
      margin: 0;
      padding: 0;
    }
    .diags li {
      display: grid;
      grid-template-columns: 64px 180px minmax(0, 1fr);
      gap: 2px 10px;
      padding: 6px 0;
      border-bottom: 1px solid var(--line-soft);
      font-size: var(--fs-sm);
    }
    .sev {
      font-weight: 600;
      text-transform: lowercase;
    }
    [data-sev='error'] .sev {
      color: var(--st-blocked);
    }
    [data-sev='warning'] .sev {
      color: var(--ink-human);
    }
    [data-sev='info'] .sev {
      color: var(--muted);
    }
    .code {
      font-family: var(--mono);
      font-size: var(--fs-xs);
      color: var(--muted);
    }
    .msg {
      color: var(--text);
    }
    .where,
    .fix {
      grid-column: 3;
      color: var(--muted);
    }
    .clean {
      color: var(--st-completed);
      display: block !important;
    }

    /* ---- history and compaction ---- */
    .history {
      margin-top: 10px;
      padding-top: 8px;
      border-top: 1px solid var(--line-soft);
      font-size: var(--fs-sm);
    }
    .hstate {
      margin: 0 0 6px;
      color: var(--text);
    }
    .pill {
      font-size: var(--fs-xs);
      font-weight: 600;
      color: var(--ink-human);
      background: var(--ink-human-wash);
      border-radius: 9px;
      padding: 0 6px;
      margin-right: 4px;
    }
    .summaries {
      list-style: none;
      margin: 0 0 6px;
      padding: 0;
    }
    .sumbtn {
      display: grid;
      grid-template-columns: auto 1fr auto;
      gap: 8px;
      width: 100%;
      border: 0;
      background: none;
      padding: 4px 6px;
      border-radius: var(--r-md);
      text-align: left;
      color: var(--text);
    }
    .sumbtn:hover,
    .open > .sumbtn {
      background: var(--raised);
    }
    .sumbtn .when {
      color: var(--text-strong);
      font-weight: 600;
      white-space: nowrap;
    }
    .sumbtn .facts {
      color: var(--muted);
      min-width: 0;
    }
    .sumbtn .by {
      color: var(--muted);
      white-space: nowrap;
    }
    .sumbody {
      padding: 6px 6px 10px;
    }
    .sumfacts {
      margin: 0 0 8px;
    }
    .linkish {
      border: 0;
      background: none;
      padding: 0;
      color: var(--focus);
      text-decoration: underline;
    }
    .hactions {
      display: flex;
      align-items: center;
      gap: 8px;
      flex-wrap: wrap;
    }
    .scrim {
      position: fixed;
      inset: 0;
      z-index: 50;
      background: rgb(10 12 16 / 0.5);
      display: grid;
      place-items: start center;
      padding-top: 8vh;
    }
    .dialog {
      width: min(620px, calc(100vw - 32px));
      max-height: 86vh;
      overflow: auto;
      background: var(--surface);
      border-radius: var(--r-lg);
      box-shadow: var(--shadow-pop);
      padding: 16px 18px 14px;
      display: flex;
      flex-direction: column;
      gap: 12px;
    }
    .dialog:focus {
      outline: none;
    }
    .dialog h2 {
      margin: 0;
    }
    .dialog p {
      margin: 0;
    }
    .dialog label {
      display: flex;
      flex-direction: column;
      gap: 4px;
      font-size: var(--fs-sm);
      color: var(--muted);
    }
    .dialog .plan {
      grid-template-columns: 120px 1fr;
      gap: 4px 12px;
      margin: 0;
    }
    .dialog .plan dd.mono {
      white-space: normal;
      overflow-wrap: anywhere;
    }
    .confirm-name {
      font-family: var(--mono);
      color: var(--text-strong);
    }
    .danger {
      padding: 8px 10px;
      border-left: 3px solid var(--st-blocked);
      background: color-mix(in srgb, var(--st-blocked) 9%, transparent);
      border-radius: var(--r-sm);
      color: var(--text);
    }
    .danger strong {
      color: var(--text-strong);
    }
    .warns {
      margin: 0;
      padding: 6px 10px 6px 26px;
      border-radius: var(--r-md);
      background: var(--ink-human-wash);
      color: var(--text);
      font-size: var(--fs-sm);
    }
    .problem {
      display: flex;
      flex-direction: column;
      gap: 4px;
      padding: 8px 10px;
      border-radius: var(--r-md);
      background: color-mix(in srgb, var(--st-blocked) 9%, transparent);
    }
    .dactions {
      display: flex;
      align-items: center;
      gap: 8px;
    }
    .btn.primary.danger-btn:not(:disabled) {
      background: var(--st-blocked);
      color: var(--bg);
    }
  `,
})
export class Health {
  protected live = inject(Live);
  private api = inject(Api);
  private toasts = inject(Toasts);
  private clock = inject(Clock);
  protected readonly now = this.clock.now;
  protected readonly ago = ago;
  protected readonly exact = exact;

  protected readonly conflicts = signal<(InboxItem & { restorable?: boolean })[]>([]);
  protected readonly diags = signal<Diagnostic[]>([]);
  protected readonly checked = signal(false);
  protected readonly checkError = signal('');
  protected readonly syncing = signal(false);
  protected readonly restoring = signal('');
  protected readonly showInfo = signal(false);

  protected readonly counts = computed(() => {
    const c = { error: 0, warning: 0, info: 0 };
    for (const d of this.diags()) c[d.severity]++;
    return c;
  });
  protected readonly shownDiags = computed(() => this.diags().filter((d) => this.showInfo() || d.severity !== 'info'));

  // ---- history summaries and compaction (spec §10.1) ----
  private router = inject(Router);
  private actions = inject(Actions);
  /** store -> its history (GET /stores/{name}/history). */
  protected readonly histories = signal<ReadonlyMap<string, HistoryOut>>(new Map());
  protected readonly historyErrors = signal<ReadonlyMap<string, string>>(new Map());
  protected readonly openSummary = signal<{ store: string; name: string } | null>(null);
  protected readonly summary = signal<HistoryEntry | null>(null);
  protected readonly summaryError = signal('');
  /** The narrative, through the shared renderer: summary text is untrusted like ticket text. */
  protected readonly summaryHtml = computed(() => renderMarkdown(this.summary()?.body));
  protected readonly compacting = signal<Compacting | null>(null);
  protected readonly narrativeText = signal('');
  protected readonly confirmText = signal('');
  /** Stores this principal may not compact (the daemon answered 403: remote mode). */
  protected readonly forbidden = signal<ReadonlySet<string>>(new Set());
  private compactForm = viewChild<ElementRef<HTMLElement>>('compactForm');
  private returnFocus?: HTMLElement;
  private summarySeq = 0;

  constructor() {
    void this.load();
    void this.loadHistories();
    // A compaction here or elsewhere changes the summaries; nothing else does.
    let compactions = this.live.historyEpoch();
    effect(() => {
      const n = this.live.historyEpoch();
      if (n === compactions) return;
      compactions = n;
      untracked(() => void this.loadHistories());
    });
    // ?store=X&summary=FILE (from an activity entry) opens that summary.
    const readLink = () => {
      const p = this.router.parseUrl(this.router.url).queryParams;
      const store = typeof p['store'] === 'string' ? p['store'] : '';
      const file = typeof p['summary'] === 'string' ? p['summary'] : '';
      if (store && file) void this.showSummary(store, file, true);
      else if (store) setTimeout(() => document.getElementById('store-' + store)?.scrollIntoView({ block: 'start' }));
    };
    readLink();
    this.router.events.pipe(takeUntilDestroyed()).subscribe((e) => {
      if (e instanceof NavigationEnd) readLink();
    });
    inject(DestroyRef).onDestroy(() => {
      if (this.actions.modal() === 'compact') this.actions.modal.set('');
    });
    // Last fetch and push times are not events; refresh them while open.
    const timer = setInterval(() => void this.refreshStores(), 10_000);
    inject(DestroyRef).onDestroy(() => clearInterval(timer));
    // Ticket and store changes may fix (or cause) findings and conflicts.
    // The 10 s refresh above only moves fetch and push times, which must not
    // re-run the (expensive) check.
    const storeState = computed(() => JSON.stringify(this.live.stores().map(({ last_fetch, last_push, ...rest }) => rest)));
    let t: ReturnType<typeof setTimeout> | undefined;
    inject(DestroyRef).onDestroy(() => clearTimeout(t));
    effect(() => {
      this.live.ticketTick();
      storeState();
      untracked(() => {
        clearTimeout(t);
        t = setTimeout(() => void this.load(), 1500);
      });
    });
  }

  private async load(): Promise<void> {
    try {
      const [sync, check] = await Promise.all([
        this.api.get<{ conflicts?: InboxItem[] }>('/sync'),
        this.api.get<{ diagnostics: Diagnostic[] }>('/check'),
      ]);
      this.conflicts.set(sync.body.conflicts ?? []);
      const order = { error: 0, warning: 1, info: 2 };
      this.diags.set([...(check.body.diagnostics ?? [])].sort((a, b) => order[a.severity] - order[b.severity] || a.path.localeCompare(b.path)));
      this.checkError.set('');
    } catch (e: any) {
      this.checkError.set(e?.message ?? String(e));
    }
    this.checked.set(true);
  }

  private async loadHistories(): Promise<void> {
    const stores = this.live.stores().map((s) => s.store);
    const results = await Promise.allSettled(stores.map((s) => this.api.get<HistoryOut>(`/stores/${encodeURIComponent(s)}/history`)));
    const h = new Map<string, HistoryOut>();
    const errs = new Map<string, string>();
    results.forEach((r, i) => {
      if (r.status === 'fulfilled') h.set(stores[i], r.value.body);
      else errs.set(stores[i], (r.reason as any)?.message ?? String(r.reason));
    });
    this.histories.set(h);
    this.historyErrors.set(errs);
  }

  protected toggleSummary(store: string, name: string): void {
    const o = this.openSummary();
    if (o?.store === store && o.name === name) {
      this.openSummary.set(null);
      return;
    }
    void this.showSummary(store, name, false);
  }

  private async showSummary(store: string, name: string, scroll: boolean): Promise<void> {
    const n = ++this.summarySeq;
    this.openSummary.set({ store, name });
    this.summary.set(null);
    this.summaryError.set('');
    if (scroll) setTimeout(() => document.getElementById(`sum-${store}-${name}`)?.scrollIntoView({ block: 'center' }));
    try {
      const r = await this.api.get<HistoryEntry>(`/stores/${encodeURIComponent(store)}/history/${encodeURIComponent(name)}`);
      if (n === this.summarySeq) this.summary.set(r.body);
    } catch (e: any) {
      if (n === this.summarySeq) this.summaryError.set(e?.message ?? String(e));
    }
  }

  protected actorsOf(h: { actors?: { [k: string]: number } }): [string, number][] {
    return Object.entries(h.actors ?? {}).sort((a, b) => b[1] - a[1]);
  }

  protected day(iso: string | undefined): string {
    if (!iso) return '?';
    const t = Date.parse(iso);
    return Number.isNaN(t) ? iso : new Date(t).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
  }

  protected bytes(n: number | undefined): string {
    if (!n) return '0 B';
    const units = ['B', 'KB', 'MB', 'GB'];
    let i = 0;
    let v = n;
    while (v >= 1024 && i < units.length - 1) {
      v /= 1024;
      i++;
    }
    return `${v < 10 && i ? v.toFixed(1) : Math.round(v)} ${units[i]}`;
  }

  protected linkClick(e: MouseEvent): void {
    followInternalLink(e, this.router);
  }

  /** Opens the dialog with a dry run: what would happen, changing nothing. */
  protected async startCompact(store: string, from: HTMLElement): Promise<void> {
    this.returnFocus = from;
    this.narrativeText.set('');
    this.confirmText.set('');
    this.compacting.set({ store });
    this.actions.modal.set('compact');
    setTimeout(() => this.compactForm()?.nativeElement.focus());
    try {
      const r = await this.api.post<CompactOut>(`/stores/${encodeURIComponent(store)}/compact`, { dry_run: true });
      if (this.compacting()?.store === store) this.compacting.set({ store, plan: r.body });
      setTimeout(() => this.compactForm()?.nativeElement.querySelector<HTMLTextAreaElement>('textarea')?.focus());
    } catch (e) {
      if (this.compacting()?.store === store) this.compacting.set({ store, problem: problemOf(e) });
    }
  }

  protected async runCompact(): Promise<void> {
    const c = this.compacting();
    if (!c?.plan || c.running || this.confirmText() !== c.store) return;
    this.compacting.set({ ...c, running: true, problem: undefined });
    try {
      const r = await this.api.post<CompactOut>(`/stores/${encodeURIComponent(c.store)}/compact`, {
        summary: this.narrativeText().trim() || undefined,
        confirm: c.store,
      });
      this.compacting.set({ store: c.store, done: r.body });
      void this.loadHistories();
      void this.refreshStores();
    } catch (e) {
      const problem = problemOf(e);
      if (problem.status === 403) {
        // Remote mode: only the daemon's local token may compact.
        this.forbidden.update((s) => new Set(s).add(c.store));
      }
      this.compacting.set({ ...c, running: false, problem });
    }
  }

  protected compactKey(e: KeyboardEvent): void {
    if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      this.closeCompact();
      return;
    }
    // Keep Tab inside the dialog.
    if (e.key === 'Tab') {
      const els = [...(this.compactForm()?.nativeElement.querySelectorAll<HTMLElement>('button:not(:disabled), textarea:not(:disabled), input:not(:disabled), a[href]') ?? [])];
      if (!els.length) return;
      const first = els[0];
      const last = els[els.length - 1];
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first.focus();
      }
    }
  }

  protected closeCompact(): void {
    if (this.compacting()?.running) return; // the rewrite must finish
    this.compacting.set(null);
    this.actions.modal.set('');
    this.returnFocus?.focus();
  }

  private async refreshStores(): Promise<void> {
    try {
      this.live.stores.set((await this.api.get<{ stores: StoreOut[] }>('/stores')).body.stores ?? []);
    } catch {
      /* the next tick retries */
    }
  }

  protected stateText(s: StoreOut): string {
    return STATE_TEXT[s.state] ?? s.state;
  }

  protected async syncNow(): Promise<void> {
    this.syncing.set(true);
    try {
      const r = await this.api.post<{ errors?: { message: string; fix?: string }[]; stores: StoreOut[]; compacted?: string[] | null; warnings?: string[] | null }>('/sync', { force: true });
      if (r.body.errors?.length) this.toasts.show(`Sync: ${r.body.errors[0].message}`, 'error', r.body.errors[0].fix);
      else if (r.body.compacted?.length) this.toasts.show(`Synced; adopted the compacted history of ${r.body.compacted.join(', ')}`);
      else this.toasts.show('Synced every store');
      for (const w of r.body.warnings ?? []) this.toasts.show(w, 'info');
      const s = await this.api.get<{ stores: StoreOut[] }>('/stores');
      this.live.stores.set(s.body.stores ?? []);
    } catch (e) {
      this.toasts.error(e, 'Sync failed');
    }
    this.syncing.set(false);
    void this.load();
  }

  protected async restore(c: InboxItem): Promise<void> {
    this.restoring.set(c.comment.name);
    try {
      await this.api.post(`/tickets/${encodeURIComponent(c.id)}/restore`, { comment: c.comment.name });
      this.toasts.show(`Restored the preserved change on ${c.id}`);
    } catch (e) {
      this.toasts.error(e, `Could not restore on ${c.id}`);
    }
    this.restoring.set('');
    void this.load();
  }
}

function problemOf(e: unknown): { message: string; fix?: string; status?: number } {
  if (e instanceof ApiError) return { message: e.message, fix: e.fix || undefined, status: e.status };
  return { message: (e as any)?.message ?? String(e) };
}
