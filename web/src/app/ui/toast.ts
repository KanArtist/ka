import { ChangeDetectionStrategy, Component, Injectable, inject, signal } from '@angular/core';

export interface Toast {
  id: number;
  text: string;
  detail?: string;
  tone: 'info' | 'error';
}

/** Short-lived notices: the result of a write, or why it failed. */
@Injectable({ providedIn: 'root' })
export class Toasts {
  readonly list = signal<Toast[]>([]);
  private n = 0;

  show(text: string, tone: Toast['tone'] = 'info', detail?: string): void {
    const t: Toast = { id: ++this.n, text, tone, detail };
    this.list.update((l) => [...l.slice(-3), t]);
    setTimeout(() => this.dismiss(t.id), tone === 'error' ? 9000 : 3500);
  }

  error(e: any, what?: string): void {
    const msg = e?.message ?? String(e);
    this.show(what ? `${what}: ${msg}` : msg, 'error', e?.fix || undefined);
  }

  dismiss(id: number): void {
    this.list.update((l) => l.filter((t) => t.id !== id));
  }
}

@Component({
  selector: 'ka-toasts',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="stack" aria-live="polite">
      @for (t of toasts.list(); track t.id) {
        <div class="toast" [class.error]="t.tone === 'error'" role="status">
          <div class="body">
            <p>{{ t.text }}</p>
            @if (t.detail) {
              <p class="fix">{{ t.detail }}</p>
            }
          </div>
          <button type="button" class="x" (click)="toasts.dismiss(t.id)" aria-label="Dismiss">×</button>
        </div>
      }
    </div>
  `,
  styles: `
    .stack {
      position: fixed;
      right: 16px;
      bottom: 16px;
      z-index: 60;
      display: flex;
      flex-direction: column;
      gap: 8px;
      max-width: min(420px, calc(100vw - 32px));
    }
    .toast {
      display: flex;
      align-items: flex-start;
      gap: 10px;
      padding: 9px 10px 9px 12px;
      border-radius: var(--r-md);
      background: var(--surface);
      box-shadow: var(--shadow-pop);
      border-left: 3px solid var(--st-completed);
      animation: in 160ms var(--ease);
    }
    .toast.error {
      border-left-color: var(--st-blocked);
    }
    .body {
      flex: 1;
      min-width: 0;
    }
    p {
      margin: 0;
      color: var(--text-strong);
      overflow-wrap: anywhere;
    }
    .fix {
      margin-top: 3px;
      color: var(--muted);
      font-size: var(--fs-sm);
    }
    .x {
      border: 0;
      background: none;
      color: var(--muted);
      font-size: 16px;
      line-height: 1;
      padding: 0 2px;
    }
    @keyframes in {
      from {
        transform: translateY(6px);
        opacity: 0;
      }
    }
  `,
})
export class ToastStack {
  protected toasts = inject(Toasts);
}
