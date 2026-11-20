import { ChangeDetectionStrategy, Component, ElementRef, Injectable, computed, effect, inject, input, signal, untracked, viewChild } from '@angular/core';
import { NavigationEnd, Router } from '@angular/router';
import { Api, SearchOut, Summary } from './api/api';
import { Live } from './live';

const RUN_DEBOUNCE_MS = 180;
const RUN_MAX_WAIT_MS = 1000;

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
  /** When the pending run was first scheduled, for the debounce's max wait. */
  private pendingSince?: number;

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
    // (a moved card may stop matching is:ready) or a question opens or is
    // answered (is:question). Other comments leave tickets() alone, so they
    // do not re-run it. A steady stream of changes delays a run by at most
    // RUN_MAX_WAIT_MS.
    effect(() => {
      const text = this.text();
      const structured = this.structured();
      this.live.tickets();
      this.live.questionTick();
      untracked(() => {
        clearTimeout(this.timer);
        if (!structured) {
          this.pendingSince = undefined;
          this.matched.set(null);
          this.error.set('');
          return;
        }
        const since = (this.pendingSince ??= Date.now());
        const wait = Math.max(0, Math.min(RUN_DEBOUNCE_MS, since + RUN_MAX_WAIT_MS - Date.now()));
        this.timer = setTimeout(() => {
          this.pendingSince = undefined;
          void this.run(text);
        }, wait);
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
        role="combobox"
        aria-autocomplete="list"
        aria-controls="qb-suggest"
        [attr.aria-expanded]="suggestions().length > 0"
        [attr.aria-activedescendant]="suggestions().length ? 'qbs-' + active() : null"
        [placeholder]="placeholder()"
        [value]="q.text()"
        (input)="q.set(box.value); caret.set(box.selectionStart ?? box.value.length); active.set(0); open.set(true)"
        (click)="caret.set(box.selectionStart ?? 0)"
        (keydown)="suggestKey($event)"
        (blur)="open.set(false)"
        aria-label="Filter with a query"
        spellcheck="false"
        autocomplete="off"
      />
      @if (q.busy()) {
        <span class="spin" aria-hidden="true"></span>
      }
    </div>
    <ul id="qb-suggest" class="suggest" role="listbox" aria-label="Query suggestions" [hidden]="!suggestions().length">
      @for (s of suggestions(); track s.value; let i = $index) {
        <li [id]="'qbs-' + i" role="option" [attr.aria-selected]="i === active()" [class.on]="i === active()" (mousedown)="$event.preventDefault(); accept(s.value)">
          <code>{{ s.value }}</code><span class="what">{{ s.hint }}</span>
        </li>
      }
    </ul>
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
    .suggest {
      position: absolute;
      top: 32px;
      left: 0;
      z-index: 6;
      width: max(100%, 360px);
      margin: 0;
      padding: 4px;
      list-style: none;
      background: var(--surface);
      border-radius: var(--r-md);
      box-shadow: var(--shadow-pop);
    }
    .suggest li {
      display: flex;
      align-items: center;
      gap: 10px;
      height: 26px;
      padding: 0 8px;
      border-radius: var(--r-sm);
      cursor: pointer;
      font-size: var(--fs-sm);
    }
    .suggest li.on {
      background: var(--raised);
    }
    .suggest code {
      font-family: var(--mono);
      color: var(--text-strong);
      white-space: nowrap;
    }
    .what {
      color: var(--muted);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
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
  private live = inject(Live);
  readonly placeholder = input('Filter: words, or project:jk kind:bug -label:infra is:ready');
  private box = viewChild.required<ElementRef<HTMLInputElement>>('box');

  // ---- suggestions: query keys, and initiatives or epics after initiative: / epic: ----
  protected readonly caret = signal(0);
  protected readonly active = signal(0);
  protected readonly open = signal(false);
  /** The term being typed: from the last space before the caret. */
  private readonly token = computed(() => {
    const before = this.q.text().slice(0, this.caret());
    const m = /(\S*)$/.exec(before);
    return m ? m[1] : '';
  });
  protected readonly suggestions = computed<{ value: string; hint: string }[]>(() => {
    if (!this.open()) return [];
    const tok = this.token();
    const neg = tok.startsWith('-') ? '-' : '';
    const t = tok.slice(neg.length).toLowerCase();
    const scope = /^(initiative|epic):(.*)$/.exec(t);
    if (scope) {
      const [, kind, rest] = scope;
      const out: { value: string; hint: string; closed: boolean }[] = [];
      for (const x of this.live.tickets().values()) {
        if (x.kind !== kind) continue;
        if (rest && !x.id.toLowerCase().startsWith(rest) && !x.title.toLowerCase().includes(rest)) continue;
        out.push({ value: `${neg}${kind}:${x.id}`, hint: x.title, closed: this.live.isClosed(x) });
      }
      out.sort((a, b) => Number(a.closed) - Number(b.closed) || a.hint.localeCompare(b.hint));
      return out.slice(0, 8).filter((o) => o.value.toLowerCase() !== tok.toLowerCase());
    }
    if (t.length < 2) return [];
    return QUERY_TERMS.filter(([term]) => term.startsWith(t) && term !== t)
      .slice(0, 8)
      .map(([term, hint]) => ({ value: neg + term, hint }));
  });

  protected suggestKey(e: KeyboardEvent): void {
    const n = this.suggestions().length;
    if (e.key === 'Escape') {
      if (n) {
        e.preventDefault();
        this.open.set(false);
      } else {
        this.box().nativeElement.blur();
      }
      return;
    }
    if (!n) return;
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      this.active.update((i) => (i + (e.key === 'ArrowDown' ? 1 : n - 1)) % n);
    } else if (e.key === 'Enter' || e.key === 'Tab') {
      e.preventDefault();
      this.accept(this.suggestions()[this.active()].value);
    }
  }

  /** Replace the term being typed with value; a key (initiative:) keeps suggesting. */
  protected accept(value: string): void {
    const el = this.box().nativeElement;
    const text = this.q.text();
    const at = this.caret();
    const start = at - this.token().length;
    const tail = text.slice(at).replace(/^\S*/, '');
    const glue = value.endsWith(':') ? '' : tail.startsWith(' ') ? '' : ' ';
    const next = text.slice(0, start) + value + glue + tail;
    this.q.set(next);
    const pos = start + value.length + glue.length;
    this.caret.set(pos);
    this.active.set(0);
    this.open.set(value.endsWith(':'));
    setTimeout(() => {
      el.value = next;
      el.setSelectionRange(pos, pos);
      el.focus();
    });
  }

  protected focusKey(e: KeyboardEvent): void {
    const t = e.target as HTMLElement | null;
    if (e.key !== 'f' || e.metaKey || e.ctrlKey || e.altKey) return;
    if (t && (t.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(t.tagName))) return;
    if (document.querySelector('ka-menu, ka-palette') || document.querySelector('ka-close-dialog form, ka-create-dialog form, ka-from-label-dialog form, [role=dialog][aria-modal=true]')) return;
    e.preventDefault();
    this.box().nativeElement.focus();
  }
}

/** Query terms the suggestions offer (docs/engine.md, Queries), with what each does. */
const QUERY_TERMS: [string, string][] = [
  ['is:ready', 'claimable now'],
  ['is:open', 'not closed'],
  ['is:closed', 'done or canceled'],
  ['is:blocked', 'blocked status or open blocker'],
  ['is:claimed', 'someone holds the claim'],
  ['is:stale', 'claim gone quiet'],
  ['is:question', 'waiting on an answer'],
  ['is:initiative', 'initiatives'],
  ['is:epic', 'epics'],
  ['is:container', 'initiatives and epics'],
  ['initiative:', 'everything below an initiative'],
  ['epic:', 'everything below an epic'],
  ['parent:', 'direct children of a ticket'],
  ['project:', 'a project'],
  ['kind:', 'bug, feature, …'],
  ['label:', 'a label'],
  ['status:', 'a status'],
  ['category:', 'started, completed, …'],
  ['priority:', 'P0 … P3'],
  ['assignee:', 'an actor (agent/* for any agent)'],
  ['has:parent', 'has a parent'],
  ['blocked_by:', 'blocked by a ticket'],
  ['blocks:', 'blocks a ticket'],
];
