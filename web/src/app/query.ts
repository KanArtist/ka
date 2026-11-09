import { ChangeDetectionStrategy, Component, ElementRef, Injectable, computed, effect, inject, input, signal, untracked, viewChild } from '@angular/core';
import { NavigationEnd, Router } from '@angular/router';
import { Api, SearchOut, Summary } from './api/api';
import { Live } from './live';

/**
 * The query shared by the Board and the Table (PRD §13.1: board and table
 * views over a shared query). It lives in the URL (?q=), so a view with a
 * query is a deep link. Plain words filter in the browser; anything in the
 * query language (project:jk kind:bug -label:infra is:ready assignee:agent/*)
 * is evaluated by the engine, so there is one implementation of it.
 */
@Injectable({ providedIn: 'root' })
export class Query {
  private api = inject(Api);
  private live = inject(Live);
  private router = inject(Router);

  /** The query text as typed. */
  readonly text = signal('');
  /** IDs the engine matched for a structured query; null when the query is plain words. */
  private readonly matched = signal<ReadonlySet<string> | null>(null);
  readonly error = signal('');
  readonly busy = signal(false);
  private seq = 0;
  private timer?: ReturnType<typeof setTimeout>;

  /** Whether the query needs the engine (has key:value terms or negations). */
  readonly structured = computed(() => /(^|\s)-?[a-z_][a-z_.]*:/i.test(this.text()));

  /** The one project the query narrows to (project:jk), or ''. */
  readonly project = computed(() => {
    const keys = [...this.text().matchAll(/(?:^|\s)project:(\S+)/gi)].map((m) => m[1]);
    return keys.length === 1 && !keys[0].includes(',') ? keys[0] : '';
  });

  constructor() {
    const read = () => {
      const q = this.router.parseUrl(this.router.url).queryParams['q'];
      const v = typeof q === 'string' ? q : '';
      if (v !== untracked(this.text)) this.text.set(v);
    };
    read();
    this.router.events.subscribe((e) => {
      if (e instanceof NavigationEnd) read();
    });
    // Re-run a structured query when it changes, and when tickets change
    // (a moved card may stop matching is:ready).
    effect(() => {
      const text = this.text();
      const structured = this.structured();
      this.live.ticketTick();
      untracked(() => {
        clearTimeout(this.timer);
        if (!structured) {
          this.matched.set(null);
          this.error.set('');
          return;
        }
        this.timer = setTimeout(() => void this.run(text), 180);
      });
    });
  }

  set(text: string): void {
    this.text.set(text);
    void this.router.navigate([], { queryParams: { q: text || null }, queryParamsHandling: 'merge', replaceUrl: true });
  }

  private async run(text: string): Promise<void> {
    const n = ++this.seq;
    this.busy.set(true);
    try {
      const r = await this.api.get<SearchOut>(`/tickets?q=${encodeURIComponent(text)}&all=true&limit=-1`);
      if (n !== this.seq) return;
      this.matched.set(new Set((r.body.tickets ?? []).map((t) => t.id)));
      this.error.set('');
    } catch (e: any) {
      if (n !== this.seq) return;
      this.error.set(e?.message ?? String(e));
      this.matched.set(new Set());
    } finally {
      if (n === this.seq) this.busy.set(false);
    }
  }

  /** The predicate both views filter with. */
  readonly matches = computed<(t: Summary) => boolean>(() => {
    const m = this.matched();
    if (m) return (t) => m.has(t.id);
    const words = this.text().trim().toLowerCase().split(/\s+/).filter(Boolean);
    if (!words.length) return () => true;
    return (t) => {
      const hay = `${t.id} ${t.title} ${(t.labels ?? []).join(' ')} ${t.assignee ?? ''} ${t.kind ?? ''}`.toLowerCase();
      return words.every((w) => hay.includes(w));
    };
  });
}

/** The query input; f focuses it. */
@Component({
  selector: 'ka-query-bar',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="qb" [class.err]="!!q.error()">
      <svg viewBox="0 0 16 16" aria-hidden="true"><circle cx="7" cy="7" r="4.5" /><path d="M10.5 10.5 14 14" /></svg>
      <input
        #box
        type="search"
        [placeholder]="placeholder()"
        [value]="q.text()"
        (input)="q.set(box.value)"
        (keydown.escape)="box.blur()"
        aria-label="Filter with a query"
        spellcheck="false"
        autocomplete="off"
      />
      @if (q.busy()) {
        <span class="spin" aria-hidden="true"></span>
      }
    </div>
    @if (q.error(); as e) {
      <p class="msg" role="alert">{{ e }}</p>
    }
  `,
  styles: `
    :host {
      position: relative;
      display: block;
    }
    .qb {
      display: flex;
      align-items: center;
      gap: 6px;
      height: 28px;
      width: var(--qbw, 320px);
      padding: 0 8px;
      border: 1px solid var(--line);
      border-radius: var(--r-md);
      background: var(--surface);
    }
    .qb:focus-within {
      border-color: var(--focus);
    }
    .qb.err {
      border-color: var(--st-blocked);
    }
    svg {
      width: 13px;
      height: 13px;
      fill: none;
      stroke: var(--muted);
      stroke-width: 1.5;
      stroke-linecap: round;
      flex: none;
    }
    input {
      flex: 1;
      min-width: 0;
      border: 0;
      background: none;
      outline: none;
      font-size: var(--fs-sm);
      font-family: var(--mono);
      color: var(--text-strong);
    }
    .spin {
      width: 10px;
      height: 10px;
      border: 1.5px solid var(--faint);
      border-top-color: transparent;
      border-radius: 50%;
      animation: spin 700ms linear infinite;
    }
    .msg {
      position: absolute;
      top: 32px;
      right: 0;
      z-index: 5;
      margin: 0;
      max-width: 420px;
      padding: 6px 9px;
      font-size: var(--fs-sm);
      color: var(--text-strong);
      background: var(--surface);
      border-radius: var(--r-md);
      box-shadow: var(--shadow-pop);
    }
    @keyframes spin {
      to {
        transform: rotate(360deg);
      }
    }
  `,
  host: { '(document:keydown)': 'focusKey($event)' },
})
export class QueryBar {
  protected q = inject(Query);
  readonly placeholder = input('Filter: words, or project:jk kind:bug -label:infra is:ready');
  private box = viewChild.required<ElementRef<HTMLInputElement>>('box');

  protected focusKey(e: KeyboardEvent): void {
    const t = e.target as HTMLElement | null;
    if (e.key !== 'f' || e.metaKey || e.ctrlKey || e.altKey) return;
    if (t && (t.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(t.tagName))) return;
    if (document.querySelector('ka-menu, ka-palette') || document.querySelector('ka-close-dialog form, ka-create-dialog form')) return;
    e.preventDefault();
    this.box().nativeElement.focus();
  }
}
