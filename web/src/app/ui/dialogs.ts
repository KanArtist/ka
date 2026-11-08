import { ChangeDetectionStrategy, Component, ElementRef, computed, effect, inject, signal, untracked, viewChild } from '@angular/core';
import { Router } from '@angular/router';
import { Actions } from '../actions';
import { Live } from '../live';

const dialogStyles = `
  .scrim {
    position: fixed;
    inset: 0;
    z-index: 50;
    background: rgb(10 12 16 / 0.5);
    display: grid;
    place-items: start center;
    padding-top: 12vh;
  }
  form {
    width: min(560px, calc(100vw - 32px));
    background: var(--surface);
    border-radius: var(--r-lg);
    box-shadow: var(--shadow-pop);
    padding: 16px 18px 14px;
    display: flex;
    flex-direction: column;
    gap: 12px;
    animation: pop 140ms var(--ease);
  }
  h2 {
    margin: 0;
    font-size: var(--fs-lg);
    font-weight: 650;
    color: var(--text-strong);
  }
  h2 .id {
    margin-right: 6px;
  }
  .row {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
  }
  .row > label {
    flex: 1;
    min-width: 110px;
  }
  label {
    display: flex;
    flex-direction: column;
    gap: 4px;
    font-size: var(--fs-sm);
    color: var(--muted);
  }
  .title-input {
    font-size: var(--fs-lg);
    font-weight: 600;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    align-items: center;
    gap: 8px;
    margin-top: 2px;
  }
  .actions .spacer {
    flex: 1;
  }
  .subtitle {
    margin: -6px 0 0;
  }
  .flush {
    margin: 0;
  }
  .chips {
    margin-top: 4px;
    display: flex;
    gap: 6px;
    flex-wrap: wrap;
  }
  .chip {
    border: 1px solid var(--line);
    background: none;
    border-radius: 14px;
    padding: 2px 10px;
    color: var(--text);
  }
  .chip[aria-pressed='true'] {
    background: var(--text-strong);
    border-color: var(--text-strong);
    color: var(--bg);
  }
  @keyframes pop {
    from {
      opacity: 0;
      transform: translateY(-6px) scale(0.99);
    }
  }
`;

/**
 * Closing a ticket records why: a resolution and a summary, committed as a
 * resolution comment (PRD §7.4). Shown whenever a move lands in a completed
 * or canceled status.
 */
@Component({
  selector: 'ka-close-dialog',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (actions.closing(); as c) {
      <div class="scrim" (click)="cancel()">
        <form (click)="$event.stopPropagation()" (submit)="$event.preventDefault(); submit()" (keydown)="key($event)" role="dialog" aria-modal="true" aria-labelledby="close-title">
          <h2 id="close-title"><span class="id">{{ c.id }}</span>Move to {{ c.status }}</h2>
          <p class="muted subtitle">{{ c.title }}</p>
          <div>
            <label id="res-label">Resolution</label>
            <div class="chips" role="radiogroup" aria-labelledby="res-label">
              @for (r of c.resolutions; track r) {
                <button type="button" class="chip" role="radio" [attr.aria-checked]="resolution() === r" [attr.aria-pressed]="resolution() === r" (click)="resolution.set(r)">{{ r }}</button>
              }
            </div>
          </div>
          <label>
            Summary: what was done, or why not
            <textarea #summary class="field" rows="4" [value]="text()" (input)="text.set(summary.value)" required></textarea>
          </label>
          <div class="actions">
            <span class="muted spacer">Saved as a resolution comment.</span>
            <button type="button" class="btn quiet" (click)="cancel()">Cancel</button>
            <button type="submit" class="btn primary" [disabled]="!text().trim()">Close ticket <kbd>⌘↵</kbd></button>
          </div>
        </form>
      </div>
    }
  `,
  styles: dialogStyles,
})
export class CloseDialog {
  protected actions = inject(Actions);
  protected readonly resolution = signal('done');
  protected readonly text = signal('');
  private summary = viewChild<ElementRef<HTMLTextAreaElement>>('summary');

  constructor() {
    effect(() => {
      const c = this.actions.closing();
      if (!c) return;
      untracked(() => {
        this.text.set('');
        this.resolution.set(c.category === 'canceled' ? (c.resolutions.find((r) => r !== 'done') ?? 'wontfix') : 'done');
        setTimeout(() => this.summary()?.nativeElement.focus());
      });
    });
  }

  protected key(e: KeyboardEvent): void {
    if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      this.cancel();
    } else if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      this.submit();
    }
  }

  protected submit(): void {
    const c = this.actions.closing();
    if (!c || !this.text().trim()) return;
    c.done({ resolution: this.resolution(), summary: this.text().trim() });
  }

  protected cancel(): void {
    this.actions.closing()?.done(null);
  }
}

/** c: a new ticket from anywhere. It opens in the peek once created. */
@Component({
  selector: 'ka-create-dialog',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (actions.creating()) {
      <div class="scrim" (click)="cancel()">
        <form (click)="$event.stopPropagation()" (submit)="$event.preventDefault(); submit()" (keydown)="key($event)" role="dialog" aria-modal="true" aria-labelledby="create-title">
          <h2 id="create-title">New ticket</h2>
          <input #title class="field title-input" type="text" placeholder="Title" aria-label="Title" [value]="titleText()" (input)="titleText.set(title.value)" maxlength="300" />
          <textarea #body class="field" rows="6" placeholder="Description (markdown)" aria-label="Description" [value]="bodyText()" (input)="bodyText.set(body.value)"></textarea>
          <div class="row">
            <label>
              Project
              <select class="field" [value]="project()" (change)="pickProject($any($event.target).value)">
                @for (p of projects(); track p.key) {
                  <option [value]="p.key" [selected]="p.key === project()">{{ p.name || p.key }}</option>
                }
              </select>
            </label>
            <label>
              Status
              <select class="field" (change)="status.set($any($event.target).value)">
                @for (s of info()?.statuses ?? []; track s.name) {
                  <option [value]="s.name" [selected]="s.name === status()">{{ s.name }}</option>
                }
              </select>
            </label>
            <label>
              Kind
              <select class="field" (change)="kind.set($any($event.target).value)">
                <option value="" [selected]="!kind()">none</option>
                @for (k of info()?.kinds ?? []; track k) {
                  <option [value]="k" [selected]="k === kind()">{{ k }}</option>
                }
              </select>
            </label>
            <label>
              Priority
              <select class="field" (change)="priority.set($any($event.target).value)">
                <option value="" [selected]="!priority()">none</option>
                @for (p of priorities(); track p) {
                  <option [value]="p" [selected]="p === priority()">{{ p }}</option>
                }
              </select>
            </label>
          </div>
          @if (error()) {
            <p class="error-text flush">{{ error() }}</p>
          }
          <div class="actions">
            <span class="muted spacer">Committed as {{ live.you() }}.</span>
            <button type="button" class="btn quiet" (click)="cancel()">Cancel</button>
            <button type="submit" class="btn primary" [disabled]="!titleText().trim() || busy()">Create <kbd>⌘↵</kbd></button>
          </div>
        </form>
      </div>
    }
  `,
  styles: dialogStyles,
})
export class CreateDialog {
  protected actions = inject(Actions);
  protected live = inject(Live);
  private router = inject(Router);
  private titleEl = viewChild<ElementRef<HTMLInputElement>>('title');

  protected readonly titleText = signal('');
  protected readonly bodyText = signal('');
  protected readonly project = signal('');
  protected readonly status = signal('');
  protected readonly kind = signal('');
  protected readonly priority = signal('');
  protected readonly busy = signal(false);
  protected readonly error = signal('');

  protected readonly projects = computed(() => (this.live.schema()?.projects ?? []).filter((p) => !p.read_only && !p.archived));
  protected readonly info = computed(() => this.projects().find((p) => p.key === this.project()));
  protected readonly priorities = computed(() => this.live.schema()?.priorities ?? ['P0', 'P1', 'P2', 'P3']);

  constructor() {
    effect(() => {
      const d = this.actions.creating();
      if (!d) return;
      untracked(() => {
        const ps = this.projects();
        const remembered = localStorage.getItem('ka.create.project') ?? '';
        const key = d.project || (ps.some((p) => p.key === remembered) ? remembered : '') || this.live.schema()?.default_project || ps[0]?.key || '';
        this.titleText.set(d.title ?? '');
        this.bodyText.set(d.body ?? '');
        this.kind.set(d.kind ?? '');
        this.priority.set(d.priority ?? '');
        this.error.set('');
        this.pickProject(key, d.status);
        setTimeout(() => this.titleEl()?.nativeElement.focus());
      });
    });
  }

  protected pickProject(key: string, status?: string): void {
    this.project.set(key);
    const p = this.projects().find((x) => x.key === key);
    this.status.set(status && p?.statuses?.some((s) => s.name === status) ? status : (p?.initial ?? ''));
    if (this.kind() && !p?.kinds?.includes(this.kind())) this.kind.set('');
  }

  protected key(e: KeyboardEvent): void {
    if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      this.cancel();
    } else if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      void this.submit();
    }
  }

  protected async submit(): Promise<void> {
    if (!this.titleText().trim() || this.busy()) return;
    this.busy.set(true);
    this.error.set('');
    const d = this.actions.creating() ?? {};
    const id = await this.actions.create({
      project: this.project(),
      title: this.titleText().trim(),
      body: this.bodyText().trim() || undefined,
      status: this.status() || undefined,
      kind: this.kind() || undefined,
      priority: this.priority() || undefined,
      parent: d.parent,
      labels: d.labels,
      assignee: d.assignee,
    });
    this.busy.set(false);
    if (!id) {
      this.error.set('The ticket was not created; see the message below.');
      return;
    }
    localStorage.setItem('ka.create.project', this.project());
    this.actions.creating.set(null);
    void this.router.navigate([], { queryParams: { peek: id }, queryParamsHandling: 'merge' });
  }

  protected cancel(): void {
    this.actions.creating.set(null);
  }
}
