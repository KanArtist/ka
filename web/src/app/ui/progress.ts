import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import type { Progress } from '../hierarchy';

interface Segment {
  key: string;
  label: string;
  n: number;
}

/** In bar order: finished work first, then what moves, then what waits. */
const PARTS: { key: keyof Progress | 'rest'; label: string }[] = [
  { key: 'done', label: 'Done' },
  { key: 'in_progress', label: 'In progress' },
  { key: 'blocked', label: 'Blocked' },
  { key: 'ready', label: 'Ready' },
  { key: 'rest', label: 'Not started' },
  { key: 'canceled', label: 'Canceled' },
];

/** "3 of 8 done, 2 in progress, 1 blocked": the bar in words. */
export function progressText(p: Progress): string {
  if (!p.total) return 'No work tickets yet';
  const parts = [`${p.done} of ${p.total} done`];
  if (p.in_progress) parts.push(`${p.in_progress} in progress`);
  if (p.blocked) parts.push(`${p.blocked} blocked`);
  if (p.ready) parts.push(`${p.ready} ready`);
  if (p.canceled) parts.push(`${p.canceled} canceled`);
  return parts.join(', ');
}

/**
 * A container's progress (spec §4.5 rollup) as one stacked bar in the
 * status colors the board already uses: done, in progress, blocked, ready,
 * not started, canceled. Compact: a short bar and done/total, for lane
 * headers and table rows. Full: a wider bar with a legend that carries
 * every count, so color is never the only label.
 */
@Component({
  selector: 'ka-progress',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (compact()) {
      <span class="compact" [title]="text()">
        <span class="bar" [class.empty]="!segments().length" role="img" [attr.aria-label]="text()">
          @for (s of segments(); track s.key) {
            <span class="seg" [attr.data-part]="s.key" [style.flex-grow]="s.n"></span>
          }
        </span>
        <span class="frac" aria-hidden="true">{{ value().done }}/{{ value().total }}</span>
      </span>
    } @else {
      <div class="full">
        <p class="head">
          <strong>{{ pct() }}%</strong>
          <span class="muted">{{ value().done }} of {{ value().total }} work tickets done</span>
        </p>
        <span class="bar big" [class.empty]="!segments().length" role="img" [attr.aria-label]="text()">
          @for (s of segments(); track s.key) {
            <span class="seg" [attr.data-part]="s.key" [style.flex-grow]="s.n"></span>
          }
        </span>
        <ul class="legend">
          @for (s of legend(); track s.key) {
            <li [class.zero]="!s.n"><span class="sw" [attr.data-part]="s.key" aria-hidden="true"></span>{{ s.label }} <b>{{ s.n }}</b></li>
          }
          <li class="total">Total <b>{{ value().total }}</b></li>
        </ul>
      </div>
    }
  `,
  styles: `
    :host {
      display: inline-block;
      min-width: 0;
    }
    :host(.block) {
      display: block;
    }
    .compact {
      display: inline-flex;
      align-items: center;
      gap: 6px;
      font-size: var(--fs-xs);
      color: var(--muted);
      white-space: nowrap;
    }
    .bar {
      display: flex;
      gap: 2px;
      width: var(--bar-w, 56px);
      height: 5px;
      border-radius: 3px;
      overflow: hidden;
    }
    .bar.big {
      width: 100%;
      height: 8px;
      border-radius: 4px;
    }
    .seg {
      flex: 0 0 0;
      min-width: 2px;
    }
    [data-part='done'] {
      background: var(--st-completed);
    }
    [data-part='in_progress'] {
      background: var(--st-started);
    }
    [data-part='blocked'] {
      background: var(--st-blocked);
    }
    [data-part='ready'] {
      background: var(--st-unstarted);
    }
    [data-part='rest'] {
      background: var(--raised);
    }
    .bar.empty {
      background: var(--raised);
    }
    [data-part='canceled'] {
      background: repeating-linear-gradient(135deg, var(--st-canceled) 0 2px, transparent 2px 4px);
    }
    .full {
      display: flex;
      flex-direction: column;
      gap: 6px;
    }
    .head {
      margin: 0;
      display: flex;
      align-items: baseline;
      gap: 8px;
      font-size: var(--fs-sm);
    }
    .head strong {
      font-size: var(--fs-lg);
      font-weight: 650;
      color: var(--text-strong);
    }
    .legend {
      list-style: none;
      margin: 0;
      padding: 0;
      display: flex;
      flex-wrap: wrap;
      gap: 2px 14px;
      font-size: var(--fs-sm);
      color: var(--muted);
    }
    .legend li {
      display: inline-flex;
      align-items: center;
      gap: 5px;
    }
    .legend li.zero {
      opacity: 0.6;
    }
    .legend b {
      font-weight: 600;
      color: var(--text-strong);
    }
    .sw {
      width: 9px;
      height: 9px;
      border-radius: 2px;
      box-shadow: inset 0 0 0 1px var(--line);
    }
    .total {
      margin-left: auto;
    }
  `,
})
export class ProgressBar {
  readonly value = input.required<Progress>();
  readonly compact = input(true);

  protected readonly text = computed(() => progressText(this.value()));
  protected readonly pct = computed(() => {
    const p = this.value();
    return p.total ? Math.round((p.done / p.total) * 100) : 0;
  });
  protected readonly legend = computed<Segment[]>(() => {
    const p = this.value();
    const rest = Math.max(0, p.total - p.done - p.in_progress - p.blocked - p.ready - p.canceled);
    return PARTS.map(({ key, label }) => ({ key, label, n: key === 'rest' ? rest : p[key] }));
  });
  /** Non-empty segments; an empty container shows the bare track. */
  protected readonly segments = computed(() => this.legend().filter((s) => s.n > 0));
}
