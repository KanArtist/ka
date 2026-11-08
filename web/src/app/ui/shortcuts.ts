import { ChangeDetectionStrategy, Component, output } from '@angular/core';

const GROUPS: { title: string; keys: [string, string][] }[] = [
  {
    title: 'Anywhere',
    keys: [
      ['⌘K  /', 'Jump to a ticket or run a command'],
      ['c', 'Create a ticket'],
      ['f', 'Filter the board or table'],
      ['g a · g b · g t', 'Activity · Board · Table'],
      ['g d · g g · g i', 'Docs · Agents · Inbox'],
      ['g h · g s', 'Sync & health · Settings'],
      ['Esc', 'Close the ticket panel'],
      ['?', 'This list'],
    ],
  },
  {
    title: 'On an open ticket',
    keys: [
      ['s', 'Status'],
      ['p', 'Priority'],
      ['a', 'Assignee'],
      ['l', 'Labels'],
      ['k', 'Kind'],
      ['e', 'Edit the description'],
      ['m', 'Comment'],
      ['⌘↵', 'Save or send'],
    ],
  },
  {
    title: 'Board and table',
    keys: [
      ['↑ ↓ ← →  j k', 'Move between cards or rows'],
      ['Shift ← →', 'Move a card to the previous or next status'],
      ['Enter', 'Open the ticket'],
    ],
  },
];

/** ?: the keyboard shortcuts (PRD §13.3). */
@Component({
  selector: 'ka-shortcuts',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="scrim" (click)="close.emit()">
      <section class="sheet" role="dialog" aria-modal="true" aria-label="Keyboard shortcuts" (click)="$event.stopPropagation()" (keydown.escape)="close.emit()" tabindex="-1">
        <h2>Keyboard shortcuts</h2>
        <div class="cols">
          @for (g of groups; track g.title) {
            <div>
              <h3>{{ g.title }}</h3>
              <dl>
                @for (k of g.keys; track k[0]) {
                  <dt><kbd>{{ k[0] }}</kbd></dt>
                  <dd>{{ k[1] }}</dd>
                }
              </dl>
            </div>
          }
        </div>
      </section>
    </div>
  `,
  styles: `
    .scrim {
      position: fixed;
      inset: 0;
      z-index: 55;
      background: rgb(10 12 16 / 0.5);
      display: grid;
      place-items: center;
    }
    .sheet {
      width: min(860px, calc(100vw - 32px));
      background: var(--surface);
      border-radius: var(--r-lg);
      box-shadow: var(--shadow-pop);
      padding: 18px 22px 22px;
      outline: none;
    }
    h2 {
      margin: 0 0 12px;
      font-size: var(--fs-lg);
      color: var(--text-strong);
    }
    .cols {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
      gap: 18px;
    }
    h3 {
      margin: 0 0 6px;
      font-size: var(--fs-sm);
      color: var(--muted);
      font-weight: 600;
    }
    dl {
      display: grid;
      grid-template-columns: max-content 1fr;
      gap: 6px 12px;
      margin: 0;
      align-items: center;
    }
    dt {
      text-align: right;
    }
    dd {
      margin: 0;
      color: var(--text);
    }
  `,
})
export class Shortcuts {
  protected readonly groups = GROUPS;
  readonly close = output<void>();
}
