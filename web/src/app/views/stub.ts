import { ChangeDetectionStrategy, Component, inject } from '@angular/core';
import { ActivatedRoute } from '@angular/router';

/** Placeholder for views that land in their own tickets (KA-9JMYK, KA-2SN5N, KA-GY86D, KA-GGM64, KA-J8XK3). */
@Component({
  selector: 'ka-stub',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `<section class="stub"><h1>{{ title }}</h1><p class="muted">This view is not built yet.</p></section>`,
  styles: `
    .stub {
      padding: 20px 24px;
    }
    h1 {
      margin: 0 0 6px;
      font-size: var(--fs-xl);
      font-weight: 650;
      color: var(--text-strong);
    }
  `,
})
export class Stub {
  protected title = inject(ActivatedRoute).snapshot.data['title'] as string;
}
