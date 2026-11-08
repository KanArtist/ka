import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { RouterLink } from '@angular/router';
import { marked } from 'marked';
import { Api, ApiError, InboxItem } from '../../api/api';
import { Live } from '../../live';
import { Prefs } from '../../prefs';
import { Actor } from '../../ui/actor';
import { Clock, ago, exact } from '../../ui/time';

type State = { busy?: boolean; error?: string; done?: string };

/**
 * Questions and approval requests addressed to you or a role you hold,
 * newest first, answered in place (PRD §13.2). Answers commit through the
 * daemon like any other operation, and the asking agent sees them in
 * ka_inbox (or its ka_ask wait returns).
 */
@Component({
  selector: 'ka-inbox',
  imports: [RouterLink, Actor],
  changeDetection: ChangeDetectionStrategy.OnPush,
  templateUrl: './inbox.html',
  styleUrl: './inbox.css',
})
export class Inbox {
  private api = inject(Api);
  protected live = inject(Live);
  protected prefs = inject(Prefs);
  private clock = inject(Clock);
  protected readonly now = this.clock.now;
  protected readonly ago = ago;
  protected readonly exact = exact;

  protected readonly states = signal<ReadonlyMap<string, State>>(new Map());
  protected readonly drafts = signal<ReadonlyMap<string, string>>(new Map());
  protected readonly notifySupported = typeof Notification !== 'undefined';
  /** What you answered here, kept on screen after the question leaves the inbox. */
  protected readonly justAnswered = signal<{ q: InboxItem; label: string }[]>([]);

  protected readonly questions = computed(() =>
    [...(this.live.inbox()?.questions ?? [])].sort((a, b) => b.comment.created.localeCompare(a.comment.created)),
  );
  protected readonly answers = computed(() =>
    [...(this.live.inbox()?.answers ?? [])].sort((a, b) => b.comment.created.localeCompare(a.comment.created)).slice(0, 20),
  );

  protected key(q: InboxItem): string {
    return q.id + '/' + q.comment.name;
  }

  protected isApproval(q: InboxItem): boolean {
    const o = q.comment.options ?? [];
    return o.length === 2 && o[0] === 'Approve' && o[1] === 'Reject';
  }

  protected html(body: string | undefined): string {
    return body ? (marked.parse(body, { gfm: true, async: false }) as string) : '';
  }

  protected draft(q: InboxItem): string {
    return this.drafts().get(this.key(q)) ?? '';
  }

  protected setDraft(q: InboxItem, v: string): void {
    this.drafts.update((m) => new Map(m).set(this.key(q), v));
  }

  protected state(q: InboxItem): State {
    return this.states().get(this.key(q)) ?? {};
  }

  private setState(q: InboxItem, s: State): void {
    this.states.update((m) => new Map(m).set(this.key(q), s));
  }

  /** choice: an option index; approved: for approvals; text: free text. */
  protected async answer(q: InboxItem, how: { choice?: number; approved?: boolean }, label: string): Promise<void> {
    const text = this.draft(q).trim();
    if (how.choice === undefined && how.approved === undefined && !text) return;
    this.setState(q, { busy: true });
    try {
      await this.api.post(`/tickets/${encodeURIComponent(q.id)}/answers`, { question: q.comment.name, ...how, text: text || undefined });
      this.setState(q, { done: label });
      this.setDraft(q, '');
      this.justAnswered.update((l) => [{ q, label }, ...l.filter((x) => this.key(x.q) !== this.key(q))].slice(0, 10));
    } catch (e) {
      const err = e instanceof ApiError ? `${e.message}${e.fix ? ' ' + e.fix : ''}` : String(e);
      this.setState(q, { error: err });
    }
  }

  protected async toggleNotify(on: boolean): Promise<void> {
    if (on && this.notifySupported && Notification.permission !== 'granted') {
      const p = await Notification.requestPermission();
      if (p !== 'granted') {
        this.prefs.notify.set(false);
        return;
      }
    }
    this.prefs.notify.set(on);
  }

  protected answerVerb(a: InboxItem): string {
    const c = a.comment;
    if (c.type === 'approval') return c.approved ? 'approved' : 'rejected';
    return 'answered';
  }
}
