import { ChangeDetectionStrategy, Component, DestroyRef, computed, effect, inject, signal, untracked } from '@angular/core';
import { RouterLink } from '@angular/router';
import { Api, InboxItem, StoreOut } from '../../api/api';
import { Live } from '../../live';
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
  imports: [RouterLink],
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
          <article class="store" [attr.data-state]="s.state">
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
              <dd>{{ s.push_mode }}</dd>
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
            <span class="code">{{ d.code }}</span>
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

  constructor() {
    void this.load();
    // Last fetch and push times are not events; refresh them while open.
    const timer = setInterval(() => void this.refreshStores(), 10_000);
    inject(DestroyRef).onDestroy(() => clearInterval(timer));
    // Ticket and store changes may fix (or cause) findings and conflicts.
    let t: ReturnType<typeof setTimeout> | undefined;
    effect(() => {
      this.live.ticketTick();
      this.live.stores();
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
      const r = await this.api.post<{ errors?: { message: string }[]; stores: StoreOut[] }>('/sync', { force: true });
      if (r.body.errors?.length) this.toasts.show(`Sync: ${r.body.errors[0].message}`, 'error');
      else this.toasts.show('Synced every store');
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
