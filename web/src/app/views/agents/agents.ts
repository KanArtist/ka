import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { RouterLink } from '@angular/router';
import { Api, ApiError, KaEvent, Session, Summary } from '../../api/api';
import { Live } from '../../live';
import { Actor, isAgent } from '../../ui/actor';
import { Clock, ago, exact } from '../../ui/time';

interface Person {
  actor: string;
  agent: boolean;
  sessions: Session[];
  claims: Summary[];
  closedToday: number;
  actionsToday: number;
  lastAt: string;
}

/**
 * Who is working on what (PRD §13.2): live sessions from presence, the
 * claims each actor holds with their age and staleness, what they finished
 * today, and one-click release for claims that went stale.
 */
@Component({
  selector: 'ka-agents',
  imports: [RouterLink, Actor],
  changeDetection: ChangeDetectionStrategy.OnPush,
  templateUrl: './agents.html',
  styleUrl: './agents.css',
})
export class Agents {
  private api = inject(Api);
  protected live = inject(Live);
  private clock = inject(Clock);
  protected readonly now = this.clock.now;
  protected readonly ago = ago;
  protected readonly exact = exact;

  private readonly today = signal<KaEvent[]>([]);
  /** Claims being released, and claims waiting for a second click. */
  protected readonly releasing = signal<ReadonlySet<string>>(new Set());
  protected readonly confirm = signal('');
  protected readonly error = signal<{ id: string; message: string } | null>(null);

  protected readonly people = computed<Person[]>(() => {
    const map = new Map<string, Person>();
    const get = (actor: string) => {
      let p = map.get(actor);
      if (!p) map.set(actor, (p = { actor, agent: isAgent(actor), sessions: [], claims: [], closedToday: 0, actionsToday: 0, lastAt: '' }));
      return p;
    };
    for (const s of this.live.presence()) {
      const p = get(s.actor);
      p.sessions.push(s);
      if (s.last_seen > p.lastAt) p.lastAt = s.last_seen;
    }
    for (const t of this.live.tickets().values()) {
      if (t.claim && !this.live.isClosed(t)) get(t.claim.by).claims.push(t);
    }
    const midnight = new Date();
    midnight.setHours(0, 0, 0, 0);
    const since = midnight.toISOString();
    const seen = new Set<string>();
    for (const e of [...this.live.events(), ...this.today()]) {
      if (!e.actor || e.at < since) continue;
      const key = `${e.type}|${e.ticket}|${e.actor}|${e.at.slice(0, 16)}`;
      if (seen.has(key)) continue;
      seen.add(key);
      if (e.type === 'presence.changed' || e.type.startsWith('sync.') || e.type.startsWith('question.')) continue;
      const p = get(e.actor);
      p.actionsToday++;
      if (e.type === 'comment.created' && e.data?.comment?.type === 'resolution') p.closedToday++;
      if (e.at > p.lastAt) p.lastAt = e.at;
    }
    for (const p of map.values()) {
      p.claims.sort((a, b) => (a.claim!.at ?? '').localeCompare(b.claim!.at ?? ''));
      p.sessions.sort((a, b) => b.last_seen.localeCompare(a.last_seen));
    }
    // Agents first; within each, connected ones, then by recent activity.
    return [...map.values()].sort(
      (a, b) =>
        Number(b.agent) - Number(a.agent) ||
        Number(b.sessions.length > 0) - Number(a.sessions.length > 0) ||
        b.lastAt.localeCompare(a.lastAt),
    );
  });

  protected readonly staleCount = computed(() => this.people().reduce((n, p) => n + p.claims.filter((c) => c.claim?.stale).length, 0));
  protected readonly connected = computed(() => this.live.presence().length);

  constructor() {
    void this.loadToday();
  }

  private async loadToday(): Promise<void> {
    try {
      const r = await this.api.get<{ events: KaEvent[] }>('/activity?limit=500');
      this.today.set(r.body.events ?? []);
    } catch {
      /* the live stream still counts what happens from now on */
    }
  }

  protected async release(t: Summary): Promise<void> {
    const id = t.id;
    if (!t.claim?.stale && this.confirm() !== id) {
      this.confirm.set(id); // a fresh claim takes a second click
      return;
    }
    this.confirm.set('');
    this.error.set(null);
    this.releasing.update((s) => new Set(s).add(id));
    try {
      await this.api.post(`/tickets/${encodeURIComponent(id)}/release`, { force: true });
    } catch (e) {
      this.error.set({ id, message: e instanceof ApiError ? `${e.message}${e.fix ? ' ' + e.fix : ''}` : String(e) });
    } finally {
      this.releasing.update((s) => {
        const n = new Set(s);
        n.delete(id);
        return n;
      });
    }
  }

  protected toolLine(s: Session): string {
    if (s.last_tool) return s.last_tool;
    return s.last_method ?? '';
  }
}
