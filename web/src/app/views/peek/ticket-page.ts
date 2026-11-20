import { ChangeDetectionStrategy, Component, inject } from '@angular/core';
import { toSignal } from '@angular/core/rxjs-interop';
import { ActivatedRoute } from '@angular/router';
import { map } from 'rxjs';
import { Peek } from './peek';

/** /ticket/:id, the deep link ka open uses: the peek as a page. */
@Component({
  selector: 'ka-ticket-page',
  imports: [Peek],
  changeDetection: ChangeDetectionStrategy.OnPush,
  // Keyed by id: /ticket/A -> /ticket/B reuses this route component, but
  // each ticket gets a fresh peek (no drafts carried over).
  template: `@for (tid of [id()]; track tid) {
    <ka-peek [id]="tid" [panel]="false" />
  }`,
})
export class TicketPage {
  protected readonly id = toSignal(inject(ActivatedRoute).paramMap.pipe(map((p) => (p.get('id') ?? '').toUpperCase())), {
    initialValue: '',
  });
}
