import { ChangeDetectionStrategy, Component, ElementRef, computed, effect, inject, output, signal, viewChild } from '@angular/core';
import { Router } from '@angular/router';
import { Summary } from '../api/api';
import { Actions } from '../actions';
import { Live } from '../live';
import { Prefs } from '../prefs';

interface Item {
  key: string;
  kind: 'ticket' | 'command';
  label: string;
  hint?: string;
  ticket?: Summary;
  run: () => void;
}

const MAX_TICKETS = 40;

/**
 * Ctrl/⌘-K. Type an ID (or its start) or words from a title to jump to a
 * ticket; commands switch views and themes. Enter runs, arrows move.
 */
@Component({
  selector: 'ka-palette',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="scrim" (click)="close.emit()"></div>
    <div class="panel" role="dialog" aria-modal="true" aria-label="Jump to a ticket or run a command">
      <input
        #q
        class="q"
        type="text"
        role="combobox"
        aria-expanded="true"
        aria-controls="palette-list"
        [attr.aria-activedescendant]="items().length ? 'pi-' + active() : null"
        placeholder="Jump to a ticket by ID or title, or type a command"
        [value]="query()"
        (input)="query.set(q.value); active.set(0)"
        (keydown)="key($event)"
        autocomplete="off"
        spellcheck="false"
      />
      <ul id="palette-list" class="list" role="listbox" aria-label="Results">
        @for (it of items(); track it.key; let i = $index) {
          <li
            [id]="'pi-' + i"
            role="option"
            [attr.aria-selected]="i === active()"
            [class.on]="i === active()"
            (mousemove)="active.set(i)"
            (click)="pick(it)"
          >
            @if (it.ticket; as t) {
              <span class="dot" [attr.data-cat]="live.category(t)"></span>
              <span class="id">{{ t.id }}</span>
              <span class="label">{{ t.title }}</span>
              <span class="hint">{{ t.status }}</span>
            } @else {
              <span class="cmd">›</span>
              <span class="label">{{ it.label }}</span>
              @if (it.hint) {
                <kbd>{{ it.hint }}</kbd>
              }
            }
          </li>
        } @empty {
          <li class="empty">No ticket or command matches “{{ query() }}”.</li>
        }
      </ul>
    </div>
  `,
  styles: `
    :host {
      position: fixed;
      inset: 0;
      z-index: 50;
      display: grid;
      justify-items: center;
      align-items: start;
      padding-top: 12vh;
    }
    .scrim {
      position: absolute;
      inset: 0;
      background: rgb(10 12 16 / 0.45);
    }
    .panel {
      position: relative;
      width: min(640px, calc(100vw - 32px));
      background: var(--surface);
      border-radius: var(--r-lg);
      box-shadow: var(--shadow-pop);
      overflow: hidden;
      animation: pop 140ms var(--ease);
    }
    @keyframes pop {
      from {
        transform: translateY(-6px) scale(0.985);
        opacity: 0;
      }
    }
    .q {
      width: 100%;
      border: 0;
      border-bottom: 1px solid var(--line);
      background: transparent;
      padding: 14px 16px;
      font-size: var(--fs-lg);
      outline: none;
    }
    .q::placeholder {
      color: var(--faint);
    }
    .list {
      list-style: none;
      margin: 0;
      padding: 6px;
      max-height: min(420px, 60vh);
      overflow: auto;
    }
    li {
      display: flex;
      align-items: center;
      gap: 10px;
      height: 34px;
      padding: 0 10px;
      border-radius: var(--r-md);
      cursor: pointer;
    }
    li.on {
      background: var(--raised);
    }
    .label {
      flex: 1;
      min-width: 0;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      color: var(--text-strong);
    }
    .hint {
      color: var(--faint);
      font-size: var(--fs-sm);
    }
    .cmd {
      width: 8px;
      color: var(--faint);
    }
    .dot {
      width: 8px;
      height: 8px;
      border-radius: 50%;
      background: var(--cat, var(--st-unstarted));
      flex: none;
    }
    .empty {
      color: var(--muted);
      cursor: default;
    }
  `,
})
export class Palette {
  readonly close = output<void>();
  protected live = inject(Live);
  private router = inject(Router);
  private prefs = inject(Prefs);
  private actions = inject(Actions);
  private input = viewChild.required<ElementRef<HTMLInputElement>>('q');

  protected readonly query = signal('');
  protected readonly active = signal(0);

  private readonly commands = computed<Item[]>(() => {
    const go = (path: string) => () => this.navigate([path]);
    return [
      {
        key: 'c:new',
        kind: 'command',
        label: 'Create a ticket',
        hint: 'C',
        run: () => {
          this.close.emit();
          this.actions.creating.set({});
        },
      },
      { key: 'c:activity', kind: 'command', label: 'Go to Activity', hint: 'G A', run: go('/activity') },
      { key: 'c:board', kind: 'command', label: 'Go to Board', hint: 'G B', run: go('/board') },
      { key: 'c:table', kind: 'command', label: 'Go to Table', hint: 'G T', run: go('/table') },
      { key: 'c:docs', kind: 'command', label: 'Go to Docs', hint: 'G D', run: go('/docs') },
      { key: 'c:health', kind: 'command', label: 'Go to Sync & health', hint: 'G H', run: go('/health') },
      { key: 'c:agents', kind: 'command', label: 'Go to Agents', hint: 'G G', run: go('/agents') },
      { key: 'c:inbox', kind: 'command', label: 'Go to Inbox', hint: 'G I', run: go('/inbox') },
      {
        key: 'c:theme',
        kind: 'command',
        label: this.prefs.theme() === 'dark' ? 'Switch to light theme' : 'Switch to dark theme',
        run: () => {
          this.prefs.toggleTheme();
          this.close.emit();
        },
      },
      {
        key: 'c:density',
        kind: 'command',
        label: this.prefs.density() === 'compact' ? 'Use comfortable density' : 'Use compact density',
        run: () => {
          this.prefs.toggleDensity();
          this.close.emit();
        },
      },
    ];
  });

  protected readonly items = computed<Item[]>(() => {
    const q = this.query().trim().toLowerCase();
    const cmds = this.commands().filter((c) => !q || c.label.toLowerCase().includes(q));
    if (!q) return [...cmds, ...this.recent()];
    return [...this.matchTickets(q), ...cmds];
  });

  constructor() {
    effect(() => this.input().nativeElement.focus());
  }

  /** With no query, the most recently updated open tickets. */
  private recent(): Item[] {
    return [...this.live.tickets().values()]
      .filter((t) => !this.live.isClosed(t))
      .sort((a, b) => (b.updated ?? '').localeCompare(a.updated ?? ''))
      .slice(0, 8)
      .map((t) => this.ticketItem(t));
  }

  private matchTickets(q: string): Item[] {
    const words = q.split(/\s+/).filter(Boolean);
    const upper = q.toUpperCase();
    const scored: [number, Summary][] = [];
    for (const t of this.live.tickets().values()) {
      let score = -1;
      if (t.id === upper) score = 0;
      else if (t.id.startsWith(upper) || t.id.split('-')[1]?.startsWith(upper)) score = 1;
      else {
        const title = t.title.toLowerCase();
        if (words.every((w) => title.includes(w))) score = title.startsWith(words[0]) ? 2 : 3;
      }
      if (score < 0) continue;
      if (this.live.isClosed(t)) score += 4; // open work first
      scored.push([score, t]);
    }
    scored.sort((a, b) => a[0] - b[0] || (b[1].updated ?? '').localeCompare(a[1].updated ?? ''));
    return scored.slice(0, MAX_TICKETS).map(([, t]) => this.ticketItem(t));
  }

  private ticketItem(t: Summary): Item {
    return { key: 't:' + t.id, kind: 'ticket', label: t.title, ticket: t, run: () => this.peekTicket(t.id) };
  }

  private peekTicket(id: string): void {
    this.close.emit();
    void this.router.navigate([], { queryParams: { peek: id }, queryParamsHandling: 'merge' });
  }

  private navigate(path: string[]): void {
    this.close.emit();
    void this.router.navigate(path);
  }

  protected pick(it: Item): void {
    it.run();
  }

  protected key(e: KeyboardEvent): void {
    const n = this.items().length;
    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault();
        if (n) this.active.update((i) => (i + 1) % n);
        break;
      case 'ArrowUp':
        e.preventDefault();
        if (n) this.active.update((i) => (i - 1 + n) % n);
        break;
      case 'Enter': {
        e.preventDefault();
        const it = this.items()[this.active()];
        if (it) it.run();
        break;
      }
      case 'Escape':
        e.preventDefault();
        this.close.emit();
        break;
    }
  }
}
