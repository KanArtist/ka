import { ChangeDetectionStrategy, Component, ElementRef, computed, effect, inject, signal, untracked, viewChild } from '@angular/core';
import { Router } from '@angular/router';
import { Actions } from '../actions';
import { Api, ApiError } from '../api/api';
import type { components } from '../api/schema';
import { Live } from '../live';
import { Toasts } from './toast';

type FromLabelOut = components['schemas']['FromLabelOut'];
type FromLabelTicket = components['schemas']['FromLabelTicket'];

/** The report's groups, in the order they are listed. */
const GROUPS: { action: FromLabelTicket['action']; title: string; note: string }[] = [
  { action: 'parented', title: 'Get the initiative as parent', note: '' },
  { action: 'epic', title: 'Epics moved under the initiative', note: 'Their labelled tickets come along.' },
  { action: 'via_epic', title: 'Under the initiative through their epic', note: 'Their own parent stays the epic.' },
  { action: 'already', title: 'Already under it', note: '' },
  { action: 'skipped', title: 'Left alone', note: 'They have another parent; move them by hand if they belong here.' },
];

/**
 * Convert a label to an initiative (ka initiative from-label): pick the
 * label, preview with a dry run (who gets parented, who comes along through
 * an epic, who is skipped and why), then confirm. The write is one create
 * and one update per ticket, pushed once.
 */
@Component({
  selector: 'ka-from-label-dialog',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (actions.fromLabel()) {
      <div class="scrim" (click)="cancel()">
        <form (click)="$event.stopPropagation()" (submit)="$event.preventDefault(); next()" (keydown)="key($event)" role="dialog" aria-modal="true" aria-labelledby="fl-title">
          <h2 id="fl-title">Convert a label to an initiative</h2>
          @if (!report()) {
            <p class="muted flush">
              Every ticket of the project carrying the label (closed ones too) gets the initiative as its parent. A ticket under an epic moves the epic instead; tickets with
              another parent are left alone. Nothing changes until you confirm the preview.
            </p>
            <div class="row">
              <label>
                Project
                <select class="field" (change)="pickProject($any($event.target).value)">
                  @for (p of projects(); track p.key) {
                    <option [value]="p.key" [selected]="p.key === project()">{{ p.name || p.key }}</option>
                  }
                </select>
              </label>
              <label>
                Label
                <select #labelSel class="field" (change)="label.set($any($event.target).value)">
                  <option value="" [selected]="!label()" disabled>Choose a label</option>
                  @for (l of labels(); track l.name) {
                    <option [value]="l.name" [selected]="l.name === label()">{{ l.name }} ({{ l.count }})</option>
                  }
                </select>
              </label>
            </div>
            <label>
              Initiative title
              <input class="field" type="text" [placeholder]="label() || 'The label'" [value]="title()" (input)="title.set($any($event.target).value)" maxlength="300" />
              <span class="hint">An initiative of the project with this title is reused.</span>
            </label>
            <label class="check"><input type="checkbox" [checked]="keep()" (change)="keep.set($any($event.target).checked)" /> Keep the label on the tickets</label>
          } @else if (report(); as r) {
            <p class="flush">
              @if (r.created) {
                Creates the initiative <strong class="ka-content">{{ r.initiative.title }}</strong> in {{ project() }}
              } @else {
                Reuses <span class="id">{{ r.initiative.id }}</span> <strong class="ka-content">{{ r.initiative.title }}</strong>
              }
              and changes {{ changes() }} {{ changes() === 1 ? 'ticket' : 'tickets' }}{{ keep() ? '' : ', removing the label “' + r.label + '” from them' }}.
            </p>
            <div class="report">
              @for (g of groups(); track g.action) {
                <section>
                  <h3>{{ g.title }} <span class="n">{{ g.items.length }}</span></h3>
                  @if (g.note) {
                    <p class="muted flush small">{{ g.note }}</p>
                  }
                  <ul>
                    @for (t of g.items; track t.id) {
                      <li>
                        <span class="id">{{ t.id }}</span>
                        <span class="t ka-content">{{ t.title }}</span>
                        @if (t.via) {
                          <span class="muted small">via {{ t.via }}</span>
                        }
                        @if (t.reason) {
                          <span class="muted small">{{ t.reason }}</span>
                        }
                      </li>
                    }
                  </ul>
                </section>
              } @empty {
                <p class="muted">No ticket of {{ project() }} carries “{{ r.label }}”.</p>
              }
            </div>
          }
          @if (problem(); as pr) {
            <div class="problem" role="alert">
              <p class="error-text flush">{{ pr.message }}</p>
              @if (pr.fix) {
                <p class="muted flush">{{ pr.fix }}</p>
              }
            </div>
          }
          <div class="actions">
            <span class="spacer"></span>
            @if (report()) {
              <button type="button" class="btn quiet" (click)="report.set(null)" [disabled]="busy()">Back</button>
            } @else {
              <button type="button" class="btn quiet" (click)="cancel()">Cancel</button>
            }
            @if (!report()) {
              <button type="submit" class="btn primary" [disabled]="!label() || busy()">{{ busy() ? 'Checking…' : 'Preview' }}</button>
            } @else {
              <button type="submit" class="btn primary" [disabled]="busy() || (!changes() && !report()!.created)">
                {{ busy() ? 'Converting…' : 'Convert ' + changes() + (changes() === 1 ? ' ticket' : ' tickets') }}
              </button>
            }
          </div>
        </form>
      </div>
    }
  `,
  styles: `
    .scrim {
      position: fixed;
      inset: 0;
      z-index: 50;
      background: rgb(10 12 16 / 0.5);
      display: grid;
      place-items: start center;
      padding-top: 10vh;
    }
    form {
      width: min(620px, calc(100vw - 32px));
      max-height: 80vh;
      overflow: auto;
      background: var(--surface);
      border-radius: var(--r-lg);
      box-shadow: var(--shadow-pop);
      padding: 16px 18px 14px;
      display: flex;
      flex-direction: column;
      gap: 12px;
    }
    h2 {
      margin: 0;
      font-size: var(--fs-lg);
      font-weight: 650;
      color: var(--text-strong);
    }
    h3 {
      margin: 0 0 2px;
      font-size: var(--fs-md);
      font-weight: 600;
      color: var(--text-strong);
    }
    .n {
      color: var(--faint);
      font-weight: 500;
    }
    .flush {
      margin: 0;
    }
    .small,
    .hint {
      font-size: var(--fs-sm);
    }
    .hint {
      color: var(--faint);
    }
    .row {
      display: flex;
      gap: 8px;
    }
    .row > label {
      flex: 1;
    }
    label {
      display: flex;
      flex-direction: column;
      gap: 4px;
      font-size: var(--fs-sm);
      color: var(--muted);
    }
    label.check {
      flex-direction: row;
      align-items: center;
      gap: 6px;
    }
    .report {
      display: flex;
      flex-direction: column;
      gap: 12px;
      border-top: 1px solid var(--line-soft);
      padding-top: 10px;
    }
    ul {
      list-style: none;
      margin: 4px 0 0;
      padding: 0;
      max-height: 180px;
      overflow: auto;
    }
    li {
      display: flex;
      align-items: baseline;
      gap: 8px;
      line-height: 22px;
      min-width: 0;
    }
    .t {
      color: var(--text-strong);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      min-width: 0;
    }
    .problem {
      display: flex;
      flex-direction: column;
      gap: 4px;
      padding: 8px 10px;
      border-radius: var(--r-md);
      background: color-mix(in srgb, var(--st-blocked) 9%, transparent);
    }
    .actions {
      display: flex;
      align-items: center;
      gap: 8px;
    }
    .spacer {
      flex: 1;
    }
  `,
})
export class FromLabelDialog {
  protected actions = inject(Actions);
  private live = inject(Live);
  private api = inject(Api);
  private toasts = inject(Toasts);
  private router = inject(Router);
  private labelSel = viewChild<ElementRef<HTMLSelectElement>>('labelSel');

  protected readonly project = signal('');
  protected readonly label = signal('');
  protected readonly title = signal('');
  protected readonly keep = signal(false);
  protected readonly busy = signal(false);
  protected readonly report = signal<FromLabelOut | null>(null);
  protected readonly problem = signal<{ message: string; fix?: string } | null>(null);

  protected readonly projects = computed(() => (this.live.schema()?.projects ?? []).filter((p) => !p.read_only && !p.archived && (p.kinds ?? []).includes('initiative')));

  /** The project's labels with how many of its tickets carry each (only while the dialog is open). */
  protected readonly labels = computed(() => {
    if (!this.actions.fromLabel()) return [];
    const key = this.project();
    const counts = new Map<string, number>();
    for (const l of this.live.schema()?.projects?.find((p) => p.key === key)?.labels ?? []) counts.set(l, 0);
    for (const t of this.live.tickets().values()) {
      if (t.project !== key) continue;
      for (const l of t.labels ?? []) counts.set(l, (counts.get(l) ?? 0) + 1);
    }
    return [...counts.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0])).map(([name, count]) => ({ name, count }));
  });

  protected readonly groups = computed(() => {
    const r = this.report();
    if (!r) return [];
    const ts = r.tickets ?? [];
    return GROUPS.map((g) => ({ ...g, items: ts.filter((t) => t.action === g.action) })).filter((g) => g.items.length);
  });
  /** Tickets the conversion writes to. */
  protected readonly changes = computed(() => (this.report()?.tickets ?? []).filter((t) => t.action === 'parented' || t.action === 'epic' || (!this.keep() && t.action !== 'skipped')).length);

  constructor() {
    effect(() => {
      const d = this.actions.fromLabel();
      if (!d) return;
      untracked(() => {
        const ps = this.projects();
        const key = (d.project && ps.some((p) => p.key === d.project) ? d.project : '') || (ps.some((p) => p.key === this.actions.viewProject()) ? this.actions.viewProject() : '') || this.live.schema()?.default_project || ps[0]?.key || '';
        this.project.set(ps.some((p) => p.key === key) ? key : (ps[0]?.key ?? ''));
        this.label.set(d.label ?? '');
        this.title.set('');
        this.keep.set(false);
        this.report.set(null);
        this.problem.set(null);
        this.busy.set(false);
        setTimeout(() => this.labelSel()?.nativeElement.focus());
      });
    });
  }

  protected pickProject(key: string): void {
    this.project.set(key);
    this.label.set('');
  }

  protected key(e: KeyboardEvent): void {
    if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      this.cancel();
    }
  }

  protected cancel(): void {
    if (this.busy() && this.report()) return; // a conversion is being written
    this.actions.fromLabel.set(null);
  }

  /** Preview (a dry run), or, on the preview, convert for real. */
  protected async next(): Promise<void> {
    if (this.busy() || !this.label()) return;
    const real = !!this.report();
    this.busy.set(true);
    this.problem.set(null);
    try {
      const body = { label: this.label(), project: this.project(), title: this.title().trim() || undefined, keep_label: this.keep(), dry_run: !real };
      const r = (await this.api.post<FromLabelOut>('/initiatives/from-label', body)).body;
      if (!real) {
        this.report.set(r);
      } else {
        this.actions.fromLabel.set(null);
        if (r.error) this.toasts.show(`Converted part of “${r.label}”: ${r.error.message}`, 'error', r.error.fix);
        else this.toasts.show(`“${r.label}” is now the initiative ${r.initiative.id}`, 'info', r.local_only || r.push_pending ? 'Committed locally; it is pushed with the next sync.' : undefined);
        if (r.initiative.id) void this.router.navigate([], { queryParams: { peek: r.initiative.id }, queryParamsHandling: 'merge' });
      }
    } catch (e) {
      const err = e instanceof ApiError ? e : null;
      this.problem.set({ message: err?.message ?? String((e as any)?.message ?? e), fix: err?.fix || undefined });
    }
    this.busy.set(false);
  }
}
