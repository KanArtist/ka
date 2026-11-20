import { Injectable, computed, inject } from '@angular/core';
import type { components } from './api/schema';
import { Summary } from './api/api';
import { Live } from './live';

export type TreeNode = components['schemas']['TreeNode'];
export type Rollup = components['schemas']['Rollup'];

/** The two container kinds (spec §4.5). Every other ticket is work. */
export type ContainerKind = 'initiative' | 'epic';

export function isContainer(kind: string | undefined | null): kind is ContainerKind {
  return kind === 'initiative' || kind === 'epic';
}

/** A ticket's nearest initiative and epic above it ('' for none). */
export interface Ancestry {
  initiative: string;
  epic: string;
}

/**
 * Progress over the work tickets below a container, from the tickets on hand.
 * The categories are disjoint, as in the engine's rollup, except that ready
 * (claimable now) is only known to the engine and stays 0 here.
 */
export interface Progress {
  total: number;
  done: number;
  canceled: number;
  in_progress: number;
  blocked: number;
  ready: number;
}

const NONE: Ancestry = { initiative: '', epic: '' };

/**
 * Initiatives and epics as the browser sees them: who is under which
 * container, computed once per change of the ticket map (one walk, memoized
 * per ticket, guarded against parent cycles), so views can lane, group and
 * show progress without walking parents per card or per row.
 */
@Injectable({ providedIn: 'root' })
export class Hierarchy {
  private live = inject(Live);

  /** id -> its nearest initiative and epic. Every ticket in the map has an entry. */
  readonly ancestry = computed<ReadonlyMap<string, Ancestry>>(() => {
    const tickets = this.live.tickets();
    const memo = new Map<string, Ancestry>();
    const visiting = new Set<string>();
    const walk = (id: string): Ancestry => {
      const known = memo.get(id);
      if (known) return known;
      const t = tickets.get(id);
      const pid = t?.parent;
      if (!t || !pid || visiting.has(id)) return NONE; // a cycle ends the walk
      const p = tickets.get(pid);
      if (!p) {
        memo.set(id, NONE);
        return NONE;
      }
      visiting.add(id);
      const above = walk(pid);
      visiting.delete(id);
      const out: Ancestry = {
        initiative: p.kind === 'initiative' ? p.id : above.initiative,
        epic: p.kind === 'epic' ? p.id : above.epic,
      };
      memo.set(id, out);
      return out;
    };
    for (const id of tickets.keys()) walk(id);
    return memo;
  });

  /** parent id -> direct children's ids. */
  readonly children = computed<ReadonlyMap<string, readonly string[]>>(() => {
    const m = new Map<string, string[]>();
    for (const t of this.live.tickets().values()) {
      if (!t.parent) continue;
      let l = m.get(t.parent);
      if (!l) m.set(t.parent, (l = []));
      l.push(t.id);
    }
    return m;
  });

  /**
   * container id -> progress over the work tickets below it. Each work
   * ticket counts once for its nearest epic and once for its initiative,
   * which is every container above it (an epic's parent can only be an
   * initiative), so this is one pass over the tickets.
   */
  readonly progress = computed<ReadonlyMap<string, Progress>>(() => {
    const anc = this.ancestry();
    const m = new Map<string, Progress>();
    const add = (id: string, t: Summary, cat: string) => {
      let p = m.get(id);
      if (!p) m.set(id, (p = { total: 0, done: 0, canceled: 0, in_progress: 0, blocked: 0, ready: 0 }));
      p.total++;
      if (cat === 'completed') p.done++;
      else if (cat === 'canceled') p.canceled++;
      else if (t.blocked) p.blocked++;
      else if (cat === 'started') p.in_progress++;
    };
    for (const t of this.live.tickets().values()) {
      if (isContainer(t.kind)) continue;
      const a = anc.get(t.id);
      if (!a || (!a.initiative && !a.epic)) continue;
      const cat = this.live.category(t);
      if (a.initiative) add(a.initiative, t, cat);
      if (a.epic) add(a.epic, t, cat);
    }
    return m;
  });

  /** The tickets above id, from the top down to its parent (cycle-safe). */
  chain(id: string): Summary[] {
    const tickets = this.live.tickets();
    const out: Summary[] = [];
    const seen = new Set<string>([id]);
    let p = tickets.get(id)?.parent;
    while (p && !seen.has(p)) {
      seen.add(p);
      const t = tickets.get(p);
      if (!t) break;
      out.unshift(t);
      p = t.parent;
    }
    return out;
  }

  /** Whether id is below ancestor (at any depth), in the tickets on hand. */
  isBelow(id: string, ancestor: string): boolean {
    const a = this.ancestry().get(id);
    if (a && (a.initiative === ancestor || a.epic === ancestor)) return true;
    return this.chain(id).some((t) => t.id === ancestor);
  }
}

/** Which parents a ticket of kind may have (spec §4.5): none, only an initiative, or any ticket. */
export function allowedParents(kind: string): 'none' | 'initiative' | 'any' {
  if (kind === 'initiative') return 'none';
  if (kind === 'epic') return 'initiative';
  return 'any';
}

/** A Rollup from the engine as a Progress. */
export function fromRollup(r: Rollup | undefined): Progress | undefined {
  if (!r) return undefined;
  return { total: r.total, done: r.done, canceled: r.canceled, in_progress: r.in_progress, blocked: r.blocked, ready: r.ready };
}

/** Whether a query asks for containers explicitly (is:container, is:epic, kind:initiative, …). */
export function asksForContainers(q: string): boolean {
  return /(^|\s)(is:(container|initiative|epic)|kind:\S*(initiative|epic))/i.test(q);
}
