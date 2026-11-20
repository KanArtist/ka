import { ChangeDetectionStrategy, Component, ElementRef, computed, effect, inject, signal, untracked, viewChild } from '@angular/core';
import { Router } from '@angular/router';
import { Actions } from '../actions';
import { ApiError } from '../api/api';
import { allowedParents, isContainer } from '../hierarchy';
import { Live } from '../live';
import { Menu, MenuOption } from './menu';
import { ActorPipe } from './actor';
import { KeysPipe } from './keys';
import { KindPipe, ResolutionPipe, StatusPipe } from './labels';

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
  imports: [KeysPipe, StatusPipe, ResolutionPipe],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (actions.closing(); as c) {
      <div class="scrim" (click)="cancel()">
        <form (click)="$event.stopPropagation()" (submit)="$event.preventDefault(); submit()" (keydown)="key($event)" role="dialog" aria-modal="true" aria-labelledby="close-title">
          <h2 id="close-title"><span class="id">{{ c.id }}</span>Move to {{ c.status | status }}</h2>
          <p class="muted subtitle">{{ c.title }}</p>
          <div>
            <label id="res-label">Resolution</label>
            <div class="chips" role="radiogroup" aria-labelledby="res-label">
              @for (r of c.resolutions; track r) {
                <button type="button" class="chip" role="radio" [attr.aria-checked]="resolution() === r" [attr.aria-pressed]="resolution() === r" (click)="resolution.set(r)">{{ r | resolution }}</button>
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
            <button type="submit" class="btn primary" [disabled]="!text().trim()">Close ticket <kbd>{{ 'Mod+Enter' | keys }}</kbd></button>
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
        // The default must be one the store allows (resolutions are configurable).
        const rs = c.resolutions;
        const pick = c.category === 'canceled' ? (rs.includes('wontfix') ? 'wontfix' : rs.find((r) => r !== 'done')) : rs.includes('done') ? 'done' : undefined;
        this.resolution.set(pick ?? rs[0] ?? '');
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
  imports: [KeysPipe, StatusPipe, ActorPipe, KindPipe, Menu],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (actions.creating()) {
      <div class="scrim" (click)="cancel()">
        <form (click)="$event.stopPropagation()" (submit)="$event.preventDefault(); submit()" (keydown)="key($event)" role="dialog" aria-modal="true" aria-labelledby="create-title">
          <h2 id="create-title">{{ kind() === 'initiative' ? 'New initiative' : kind() === 'epic' ? 'New epic' : 'New ticket' }}</h2>
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
                  <option [value]="s.name" [selected]="s.name === status()">{{ s.name | status }}</option>
                }
              </select>
            </label>
            <label>
              Kind
              <select class="field" (change)="kind.set($any($event.target).value)">
                <option value="" [selected]="!kind()">None</option>
                @for (k of info()?.kinds ?? []; track k) {
                  <option [value]="k" [selected]="k === kind()">{{ k | kind }}</option>
                }
              </select>
            </label>
            <label>
              Priority
              <select class="field" (change)="priority.set($any($event.target).value)">
                <option value="" [selected]="!priority()">None</option>
                @for (p of priorities(); track p) {
                  <option [value]="p" [selected]="p === priority()">{{ p }}</option>
                }
              </select>
            </label>
          </div>
          <div class="row">
            <label class="parent-field">
              <span id="parent-label">Parent</span>
              <button
                #parentBtn
                type="button"
                class="field parent-btn"
                aria-labelledby="parent-label"
                aria-haspopup="dialog"
                [attr.aria-describedby]="parentNote() ? 'parent-note' : null"
                [disabled]="parentRule() === 'none'"
                (click)="parentMenu.set(true)"
              >
                @if (parentTicket(); as p) {
                  <span class="dot" [attr.data-cat]="live.category(p)"></span>
                  @if (p.kind === 'initiative' || p.kind === 'epic') {
                    <span class="pkind">{{ p.kind | kind }}</span>
                  }
                  <span class="id">{{ p.id }}</span>
                  <span class="ptitle">{{ p.title }}</span>
                } @else if (parent()) {
                  <span class="id">{{ parent() }}</span>
                } @else {
                  <span class="muted">{{ parentRule() === 'none' ? 'None: an initiative is top level' : 'None' }}</span>
                }
              </button>
            </label>
          </div>
          @if (parentNote()) {
            <p id="parent-note" class="muted flush note">{{ parentNote() }}</p>
          }
          @if (problem(); as pr) {
            <div class="problem" role="alert">
              <p class="error-text flush">{{ pr.message }}</p>
              @if (pr.fix) {
                <p class="muted flush">{{ pr.fix }}</p>
              }
              @if (pr.code === 'containers_unguarded' && storeOf()) {
                <p class="muted flush">
                  Initiatives and epics need every client of <strong>{{ storeOf() }}</strong> on ka 1.3.0 or newer. Once they are, raise the store's requirement (the same as
                  <code>ka store require 1.3.0 --store {{ storeOf() }}</code>), then create again.
                </p>
                <div>
                  <button type="button" class="btn small" (click)="requireContainers()" [disabled]="busy()">Require ka 1.3.0 in {{ storeOf() }}…</button>
                </div>
              }
            </div>
          }
          <div class="actions">
            <span class="muted spacer">Committed as <span [title]="live.you()">{{ live.you() | actor }}</span>.</span>
            <button type="button" class="btn quiet" (click)="cancel()">Cancel</button>
            <button type="submit" class="btn primary" [disabled]="!titleText().trim() || busy()">Create <kbd>{{ 'Mod+Enter' | keys }}</kbd></button>
          </div>
        </form>
      </div>
    }
    @if (parentMenu() && actions.creating()) {
      <ka-menu
        [options]="parentOptions()"
        [selected]="parent()"
        [anchor]="parentBtn()?.nativeElement ?? null"
        [limit]="60"
        [width]="420"
        [label]="parentRule() === 'initiative' ? 'Choose an initiative' : 'Choose a parent'"
        [placeholder]="parentRule() === 'initiative' ? 'Find an initiative…' : 'Find a ticket, epic or initiative…'"
        (pickValue)="parent.set($event); parentNote.set('')"
        (close)="closeParentMenu()"
      />
    }
  `,
  styles:
    dialogStyles +
    `
    /* The menu opens over the dialog. */
    ka-menu {
      position: relative;
      z-index: 55;
    }
    .parent-btn {
      display: flex;
      align-items: center;
      gap: 7px;
      height: 30px;
      padding: 0 9px;
      text-align: left;
      min-width: 0;
    }
    .parent-btn:disabled {
      opacity: 0.7;
      cursor: default;
    }
    .ptitle {
      min-width: 0;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      font-family: var(--content);
    }
    .pkind {
      font-size: var(--fs-xs);
      font-weight: 600;
      border: 1px solid var(--line);
      border-radius: var(--r-sm);
      padding: 0 4px;
      line-height: 15px;
      color: var(--muted);
    }
    .dot {
      width: 8px;
      height: 8px;
      border-radius: 50%;
      flex: none;
    }
    .note {
      margin-top: -6px;
      font-size: var(--fs-sm);
    }
    .problem {
      display: flex;
      flex-direction: column;
      gap: 6px;
      padding: 8px 10px;
      border-radius: var(--r-md);
      background: color-mix(in srgb, var(--st-blocked) 9%, transparent);
    }
  `,
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
  protected readonly problem = signal<{ message: string; fix?: string; code?: string } | null>(null);

  // ---- parent (spec §4.5: initiative > epic > ticket) ----
  protected readonly parent = signal('');
  protected readonly parentNote = signal('');
  protected readonly parentMenu = signal(false);
  protected readonly parentBtn = viewChild<ElementRef<HTMLButtonElement>>('parentBtn');
  protected readonly parentRule = computed(() => allowedParents(this.kind()));
  protected readonly parentTicket = computed(() => this.live.tickets().get(this.parent()));
  /**
   * Valid parents for the chosen kind: initiatives for an epic; for work,
   * initiatives and epics, then open tickets (this project's first). Open
   * before closed; the menu shows the first matches and narrows as you type.
   */
  protected readonly parentOptions = computed<MenuOption[]>(() => {
    if (!this.parentMenu()) return [];
    const rule = this.parentRule();
    const project = this.project();
    const rank = (k: string | undefined, p: string) => (k === 'initiative' ? 0 : k === 'epic' ? 1 : p === project ? 2 : 3);
    const items: [number, MenuOption][] = [];
    for (const t of this.live.tickets().values()) {
      const closed = this.live.isClosed(t);
      if (rule === 'initiative' && t.kind !== 'initiative') continue;
      if (rule === 'any' && !isContainer(t.kind) && closed) continue;
      const r = rank(t.kind, t.project) + (closed ? 10 : 0);
      const kind = t.kind === 'initiative' ? 'Initiative: ' : t.kind === 'epic' ? 'Epic: ' : '';
      items.push([r, { value: t.id, label: kind + t.title, hint: t.id, cat: this.live.category(t) }]);
    }
    items.sort((a, b) => a[0] - b[0] || a[1].label.localeCompare(b[1].label));
    return [{ value: '', label: 'No parent' }, ...items.map(([, o]) => o)];
  });
  /** The store of the chosen project (for containers_unguarded). */
  protected readonly storeOf = computed(() => this.info()?.store ?? '');

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
        const writable = (k: string) => (ps.some((p) => p.key === k) ? k : '');
        // The project asked for, then the one in view, then the last one used.
        const key =
          d.project ||
          writable(this.actions.viewProject()) ||
          writable(remembered) ||
          this.live.schema()?.default_project ||
          ps[0]?.key ||
          '';
        this.titleText.set(d.title ?? '');
        this.bodyText.set(d.body ?? '');
        this.kind.set(d.kind ?? '');
        this.priority.set(d.priority ?? '');
        this.parent.set(d.parent ?? '');
        this.parentNote.set('');
        this.parentMenu.set(false);
        this.problem.set(null);
        this.pickProject(key, d.status);
        setTimeout(() => this.titleEl()?.nativeElement.focus());
      });
    });
    // A kind change can leave the parent breaking the nesting rules: say so and clear it.
    effect(() => {
      const rule = this.parentRule();
      untracked(() => {
        const p = this.parent();
        if (!p) return;
        const pk = this.live.tickets().get(p)?.kind;
        if (rule === 'none') {
          this.parent.set('');
          this.parentNote.set('An initiative has no parent, so the parent was cleared.');
        } else if (rule === 'initiative' && pk !== undefined && pk !== 'initiative') {
          this.parent.set('');
          this.parentNote.set("An epic's parent must be an initiative, so the parent was cleared.");
        }
      });
    });
  }

  protected closeParentMenu(): void {
    this.parentMenu.set(false);
    setTimeout(() => this.parentBtn()?.nativeElement.focus());
  }

  protected async requireContainers(): Promise<void> {
    const store = this.storeOf();
    if (!store) return;
    if (!confirm(`Raise ${store}'s requires_ka to 1.3.0?\n\nClients older than ka 1.3.0 will refuse the store until they upgrade. Do this only once everyone using it runs 1.3.0. It is one commit to .ka/store.yml, pushed like other changes.`)) return;
    this.busy.set(true);
    const ok = await this.actions.requireKa(store, '1.3.0');
    this.busy.set(false);
    if (ok) this.problem.set(null);
  }

  protected pickProject(key: string, status?: string): void {
    this.project.set(key);
    const p = this.projects().find((x) => x.key === key);
    this.status.set(status && p?.statuses?.some((s) => s.name === status) ? status : (p?.initial ?? ''));
    if (this.kind() && !p?.kinds?.includes(this.kind())) this.kind.set('');
  }

  protected key(e: KeyboardEvent): void {
    if (this.parentMenu()) return; // the menu has the keys
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
    this.problem.set(null);
    const d = this.actions.creating() ?? {};
    let id = '';
    try {
      id = await this.actions.createOrThrow({
        project: this.project(),
        title: this.titleText().trim(),
        body: this.bodyText().trim() || undefined,
        status: this.status() || undefined,
        kind: this.kind() || undefined,
        priority: this.priority() || undefined,
        parent: this.parent() || undefined,
        labels: d.labels,
        assignee: d.assignee,
      });
    } catch (e) {
      const err = e instanceof ApiError ? e : null;
      this.problem.set({ message: err?.message ?? String((e as any)?.message ?? e), fix: err?.fix || undefined, code: err?.code });
    }
    this.busy.set(false);
    if (!id) {
      if (!this.problem()) this.problem.set({ message: 'The ticket was not created.' });
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
