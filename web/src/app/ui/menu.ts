import { ChangeDetectionStrategy, Component, ElementRef, HostListener, afterNextRender, computed, input, output, signal, viewChild } from '@angular/core';
import { Avatar } from './actor';

export interface MenuOption {
  value: string;
  label: string;
  hint?: string;
  /** Status category, for a colored dot. */
  cat?: string;
  /** Show an actor avatar. */
  actor?: string;
  /** Priority, for its color. */
  prio?: string;
}

/**
 * A popover list for picking a value (status, priority, assignee) or
 * toggling several (labels). Type to filter, arrows to move, Enter to pick,
 * Esc to close. It positions itself against its anchor element, flipping
 * up near the bottom of the window.
 */
@Component({
  selector: 'ka-menu',
  imports: [Avatar],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="scrim" (click)="close.emit()" (contextmenu)="$event.preventDefault(); close.emit()"></div>
    <div class="menu" role="dialog" [attr.aria-label]="label()" [style.top.px]="pos().top" [style.left.px]="pos().left" [style.bottom.px]="pos().bottom">
      <input
        #q
        class="q"
        type="text"
        role="combobox"
        aria-expanded="true"
        aria-controls="menu-list"
        [attr.aria-activedescendant]="shown().length ? 'mo-' + active() : null"
        [placeholder]="placeholder()"
        [value]="query()"
        (input)="query.set(q.value); active.set(0)"
        (keydown)="key($event)"
        autocomplete="off"
        spellcheck="false"
      />
      <ul id="menu-list" role="listbox" [attr.aria-multiselectable]="multi() || null" [attr.aria-label]="label()">
        @for (o of shown(); track o.value; let i = $index) {
          <li
            [id]="'mo-' + i"
            role="option"
            [attr.aria-selected]="isOn(o.value)"
            [class.active]="i === active()"
            (mousemove)="active.set(i)"
            (click)="pick(o)"
          >
            <span class="check" aria-hidden="true">{{ isOn(o.value) ? '✓' : '' }}</span>
            @if (o.cat) {
              <span class="dot" [attr.data-cat]="o.cat"></span>
            }
            @if (o.actor) {
              <ka-avatar [actor]="o.actor" [size]="16" />
            }
            <span class="label" [attr.data-p]="o.prio || null">{{ o.label }}</span>
            @if (o.hint) {
              <span class="hint">{{ o.hint }}</span>
            }
            @if (i < 9 && !query()) {
              <kbd>{{ i + 1 }}</kbd>
            }
          </li>
        } @empty {
          <li class="none">No matches</li>
        }
      </ul>
    </div>
  `,
  styles: `
    .scrim {
      position: fixed;
      inset: 0;
      z-index: 40;
    }
    .menu {
      position: fixed;
      z-index: 41;
      width: 248px;
      max-height: min(360px, 60vh);
      display: flex;
      flex-direction: column;
      background: var(--surface);
      border-radius: var(--r-lg);
      box-shadow: var(--shadow-pop);
      overflow: hidden;
      animation: pop 110ms var(--ease);
    }
    .q {
      border: 0;
      border-bottom: 1px solid var(--line-soft);
      background: none;
      padding: 9px 12px;
      outline: none;
      color: var(--text-strong);
    }
    ul {
      list-style: none;
      margin: 0;
      padding: 4px;
      overflow: auto;
    }
    li {
      display: flex;
      align-items: center;
      gap: 8px;
      height: 30px;
      padding: 0 8px;
      border-radius: var(--r-md);
      cursor: pointer;
      color: var(--text);
    }
    li.active {
      background: var(--raised);
      color: var(--text-strong);
    }
    li.none {
      color: var(--muted);
      cursor: default;
    }
    .check {
      width: 12px;
      color: var(--text-strong);
      font-size: var(--fs-sm);
    }
    .dot {
      width: 8px;
      height: 8px;
      border-radius: 50%;
      flex: none;
    }
    .label {
      flex: 1;
      min-width: 0;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    .label[data-p='P0'] {
      color: var(--prio-0);
    }
    .label[data-p='P1'] {
      color: var(--prio-1);
    }
    .hint {
      color: var(--faint);
      font-size: var(--fs-sm);
    }
    @keyframes pop {
      from {
        opacity: 0;
        transform: translateY(-3px);
      }
    }
  `,
})
export class Menu {
  readonly options = input.required<MenuOption[]>();
  readonly selected = input<string | readonly string[]>('');
  readonly multi = input(false);
  /** Offer the typed text as a new value (labels, when the project allows any). */
  readonly allowCustom = input(false);
  readonly anchor = input<HTMLElement | null>(null);
  readonly label = input('Choose');
  readonly placeholder = input('Filter…');
  readonly pickValue = output<string>();
  readonly close = output<void>();

  private q = viewChild<ElementRef<HTMLInputElement>>('q');
  protected readonly query = signal('');
  protected readonly active = signal(0);
  protected readonly pos = signal<{ top?: number; left: number; bottom?: number }>({ top: 0, left: 0 });

  protected readonly shown = computed<MenuOption[]>(() => {
    const q = this.query().trim().toLowerCase();
    const all = this.options();
    const out = q ? all.filter((o) => o.label.toLowerCase().includes(q) || o.value.toLowerCase().includes(q)) : all;
    if (q && this.allowCustom() && !all.some((o) => o.value.toLowerCase() === q)) {
      return [...out, { value: this.query().trim(), label: `Add “${this.query().trim()}”` }];
    }
    return out;
  });

  constructor() {
    afterNextRender(() => {
      this.place();
      this.q()?.nativeElement.focus();
      const sel = this.selected();
      const i = typeof sel === 'string' ? this.options().findIndex((o) => o.value === sel) : -1;
      if (i >= 0) this.active.set(i);
    });
  }

  @HostListener('window:resize')
  protected place(): void {
    const a = this.anchor();
    if (!a) return;
    const r = a.getBoundingClientRect();
    const left = Math.max(8, Math.min(r.left, window.innerWidth - 256));
    if (window.innerHeight - r.bottom < 300 && r.top > window.innerHeight - r.bottom) {
      this.pos.set({ left, bottom: window.innerHeight - r.top + 4 });
    } else {
      this.pos.set({ left, top: r.bottom + 4 });
    }
  }

  protected isOn(v: string): boolean {
    const s = this.selected();
    return typeof s === 'string' ? s === v : s.includes(v);
  }

  protected pick(o: MenuOption): void {
    this.pickValue.emit(o.value);
    if (!this.multi()) this.close.emit();
  }

  protected key(e: KeyboardEvent): void {
    const n = this.shown().length;
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      this.active.update((i) => (n ? (i + 1) % n : 0));
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      this.active.update((i) => (n ? (i - 1 + n) % n : 0));
    } else if (e.key === 'Enter') {
      e.preventDefault();
      const o = this.shown()[this.active()];
      if (o) this.pick(o);
    } else if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      this.close.emit();
    } else if (!this.query() && /^[1-9]$/.test(e.key)) {
      const o = this.shown()[Number(e.key) - 1];
      if (o) {
        e.preventDefault();
        this.pick(o);
      }
    }
  }
}
