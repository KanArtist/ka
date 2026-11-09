import { KaEvent } from '../../api/api';
import { actorName } from '../../ui/actor';
import { fieldLabel, humanize, kindLabel, resolutionLabel, statusLabel } from '../../ui/labels';

/** The filter group an event belongs to. */
export type Kind = 'tickets' | 'comments' | 'claims' | 'questions' | 'sync' | 'sessions';

export interface Line {
  verb: string; // "moved to review"
  quote?: string; // a comment's text
  tone?: 'done' | 'warn' | 'question';
}

const FIELD_NAMES: Record<string, string> = {
  priority: 'priority',
  kind: 'kind',
  assignee: 'assignee',
  owner: 'owner',
  parent: 'parent',
  title: 'title',
  labels: 'labels',
  blocked_by: 'blockers',
  related: 'related tickets',
  links: 'links',
  acceptance: 'acceptance',
  due: 'due date',
  milestone: 'milestone',
};

export function kindOf(e: KaEvent): Kind | null {
  switch (e.type) {
    case 'ticket.created':
    case 'ticket.updated':
    case 'ticket.moved':
    case 'ticket.removed':
    case 'commit':
      return 'tickets';
    case 'comment.created':
      return ['question', 'answer', 'approval'].includes(e.data?.comment?.type) ? 'questions' : 'comments';
    case 'claim.changed':
      return 'claims';
    case 'sync.state':
    case 'sync.conflict_preserved':
    case 'store.changed':
      return 'sync';
    case 'presence.changed':
      return 'sessions';
    default:
      // question.* repeat comment.created; reset is plumbing.
      return null;
  }
}

/** One line of activity, in plain words. */
export function describe(e: KaEvent): Line {
  const d = e.data ?? {};
  switch (e.type) {
    case 'ticket.created':
      return { verb: 'created it' };
    case 'ticket.moved':
      return {
        verb: d.from ? `moved it from ${statusLabel(d.from)} to ${statusLabel(d.to)}` : `moved it to ${statusLabel(d.to ?? d.ticket?.status)}`,
        tone: d.category === 'completed' || d.to === 'done' ? 'done' : undefined,
      };
    case 'ticket.removed':
      return { verb: 'removed it' };
    case 'ticket.updated':
      return { verb: describeChanges(d) };
    case 'claim.changed':
      return d.to ? { verb: 'claimed it' } : { verb: 'released it' };
    case 'comment.created':
      return describeComment(d.comment ?? {});
    case 'sync.state':
      return {
        verb: `${e.store} is ${d.state}${d.detail ? ` (${d.detail})` : ''}`,
        tone: d.state === 'synced' ? undefined : 'warn',
      };
    case 'sync.conflict_preserved':
      return { verb: `kept ${d.preserved?.length ?? 1} conflicting edit(s) as comments`, tone: 'warn' };
    case 'store.changed':
      return { verb: `changed the configuration of ${e.store}` };
    case 'presence.changed':
      if (d.state === 'connected') return { verb: `connected${d.session?.client ? ` from ${humanize(d.session.client)}` : ''}` };
      return { verb: 'disconnected' };
    case 'commit':
      return { verb: e.text || 'committed' };
    default:
      return { verb: e.text || e.type };
  }
}

/** A field's name inside a sentence: "priority", "Legacy ID". */
function fieldName(k: string): string {
  return FIELD_NAMES[k] ?? fieldLabel(k);
}

/** A field's new value as people read it. */
function valueText(k: string, v: unknown): string {
  const s = String(v);
  if (k === 'assignee' || k === 'owner') return actorName(s);
  if (k === 'kind') return kindLabel(s);
  if (k === 'status') return statusLabel(s);
  return s;
}

function describeChanges(d: any): string {
  const changes: Record<string, any> = d.changes ?? {};
  const parts: string[] = [];
  for (const [k, c] of Object.entries(changes)) {
    if (k === 'acceptance') {
      const count = typeof c?.to === 'string' && /^\d+\/\d+$/.test(c.to) ? c.to : d.ticket?.acceptance;
      parts.push(count ? `checked acceptance (${count})` : 'changed acceptance');
      continue;
    }
    const to = c?.to;
    if (to && typeof to === 'object' && !Array.isArray(to)) {
      const add = (to.add ?? []).map((v: string) => '+' + v);
      const rm = (to.remove ?? []).map((v: string) => '−' + v);
      parts.push(`${fieldName(k)} ${[...add, ...rm].join(' ')}`);
    } else if (Array.isArray(to) || Array.isArray(c?.from)) {
      const from = new Set<string>(c?.from ?? []);
      const now = new Set<string>(to ?? []);
      const add = [...now].filter((v) => !from.has(v)).map((v) => '+' + v);
      const rm = [...from].filter((v) => !now.has(v)).map((v) => '−' + v);
      parts.push(`${fieldName(k)} ${[...add, ...rm].join(' ')}`.trim());
    } else if (to === null || to === undefined || to === '') {
      parts.push(`cleared ${fieldName(k)}`);
    } else if (typeof to === 'object') {
      parts.push(`changed ${fieldName(k)}`);
    } else {
      parts.push(`set ${fieldName(k)} to ${valueText(k, to)}`);
    }
  }
  if (d.body_changed) parts.push('edited the description');
  return parts.length ? parts.join(', ') : 'updated it';
}

function describeComment(c: any): Line {
  const quote = firstLines(c.body);
  switch (c.type) {
    case 'progress':
      return { verb: 'posted progress', quote };
    case 'decision':
      return { verb: 'recorded a decision', quote };
    case 'resolution':
      return { verb: `closed it as ${resolutionLabel(c.resolution ?? 'done').toLowerCase()}`, quote, tone: 'done' };
    case 'question':
      return { verb: c.to ? `asked ${actorName(c.to)}` : 'asked a question', quote, tone: 'question' };
    case 'answer':
      return { verb: c.choice_text ? `answered “${c.choice_text}”` : 'answered', quote };
    case 'approval':
      return { verb: c.approved ? 'approved' : 'rejected', quote, tone: c.approved ? 'done' : 'warn' };
    case 'system':
      return { verb: 'kept a conflicting edit', quote, tone: 'warn' };
    default:
      return { verb: 'commented', quote };
  }
}

function firstLines(body: string | undefined): string | undefined {
  if (!body) return undefined;
  const t = body.trim().replace(/\s+/g, ' ');
  return t.length > 240 ? t.slice(0, 237) + '…' : t;
}
