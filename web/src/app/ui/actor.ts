import { ChangeDetectionStrategy, Component, Pipe, PipeTransform, computed, input, signal } from '@angular/core';
import type { ActorOut } from '../api/api';
import { humanize } from './labels';

export function isAgent(actor: string | undefined | null): boolean {
  return !!actor && actor.startsWith('agent/');
}

/** Display names from the actor registries (.ka/actors.yml), by handle and alias. */
const directory = signal<ReadonlyMap<string, string>>(new Map());

export function setActors(actors: ActorOut[]): void {
  const m = new Map<string, string>();
  for (const a of actors) {
    if (!a.name) continue;
    m.set(a.handle, a.name);
    for (const al of a.aliases ?? []) if (!m.has(al)) m.set(al, a.name);
  }
  directory.set(m);
}

/** Names for well-known agents that the registry doesn't name. */
const KNOWN_AGENTS: Record<string, string> = {
  claude: 'Claude',
  'claude-code': 'Claude Code',
  codex: 'Codex',
  agy: 'Antigravity',
  antigravity: 'Antigravity',
  gemini: 'Gemini',
  cursor: 'Cursor',
  copilot: 'Copilot',
  aider: 'Aider',
  amp: 'Amp',
  opencode: 'OpenCode',
};

/**
 * The name people read for an actor: the registry's name: field, then a
 * well-known agent's name, then the handle made readable (roles and agents)
 * or the handle as is (people, whose handles are often their login). Show
 * the handle itself in a tooltip.
 */
export function actorName(actor: string | undefined | null): string {
  if (!actor) return 'someone';
  const named = directory().get(actor);
  if (named) return named;
  if (actor.startsWith('role/')) return humanize(actor.slice('role/'.length));
  if (isAgent(actor)) {
    const slug = actor.slice('agent/'.length);
    return KNOWN_AGENTS[slug.toLowerCase()] ?? humanize(slug);
  }
  return actor;
}

/** {{ handle | actor }}: the actor's display name. Impure: names arrive after the first render. */
@Pipe({ name: 'actor', pure: false })
export class ActorPipe implements PipeTransform {
  transform(actor: string | undefined | null): string {
    return actorName(actor);
  }
}

/**
 * An actor's mark. Agents get a hexagon in verdigris with a robot glyph;
 * humans get a circle in ochre with their initial. The shape carries the
 * meaning too, so it does not depend on color alone.
 */
@Component({
  selector: 'ka-avatar',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <span class="av" [class.agent]="agent()" [class.human]="!agent()" [style.--sz.px]="size()" [attr.title]="actor()" aria-hidden="true">
      @if (agent()) {
        <svg viewBox="0 0 16 16" class="glyph">
          <rect x="3.5" y="5" width="9" height="7" rx="2" fill="none" stroke="currentColor" stroke-width="1.4" />
          <path d="M8 2.6V5" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" />
          <circle cx="6.3" cy="8.4" r="1" fill="currentColor" />
          <circle cx="9.7" cy="8.4" r="1" fill="currentColor" />
        </svg>
      } @else {
        <span class="initial">{{ initial() }}</span>
      }
    </span>
  `,
  styles: `
    :host {
      display: inline-flex;
      vertical-align: middle;
    }
    .av {
      --sz: 20px;
      width: var(--sz);
      height: var(--sz);
      display: inline-grid;
      place-items: center;
      flex: none;
    }
    .human {
      border-radius: 50%;
      color: var(--ink-human);
      background: var(--ink-human-wash);
      box-shadow: inset 0 0 0 1.5px var(--ink-human);
    }
    .agent {
      color: var(--ink-agent);
      background: var(--ink-agent);
      clip-path: polygon(25% 3%, 75% 3%, 100% 50%, 75% 97%, 25% 97%, 0 50%);
      position: relative;
    }
    .agent::before {
      content: '';
      position: absolute;
      inset: 1.5px;
      background: color-mix(in srgb, var(--ink-agent) 16%, var(--surface));
      clip-path: inherit;
    }
    .glyph {
      position: relative;
      width: 62%;
      height: 62%;
    }
    .initial {
      font-size: calc(var(--sz) * 0.5);
      font-weight: 650;
      line-height: 1;
    }
  `,
})
export class Avatar {
  readonly actor = input.required<string>();
  readonly size = input(20);
  protected readonly agent = computed(() => isAgent(this.actor()));
  protected readonly name = computed(() => actorName(this.actor()));
  protected readonly initial = computed(() => this.name().charAt(0).toUpperCase());
}

/** An avatar plus the actor's name, in their ink. */
@Component({
  selector: 'ka-actor',
  imports: [Avatar],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `<ka-avatar [actor]="actor()" [size]="size()" /><span class="name" [class.agent]="agent()" [attr.title]="actor()">{{ name() }}</span>`,
  styles: `
    :host {
      display: inline-flex;
      align-items: center;
      gap: 6px;
      min-width: 0;
    }
    .name {
      color: var(--ink-human);
      font-weight: 550;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    .name.agent {
      color: var(--ink-agent);
    }
  `,
})
export class Actor {
  readonly actor = input.required<string>();
  readonly size = input(18);
  protected readonly agent = computed(() => isAgent(this.actor()));
  protected readonly name = computed(() => actorName(this.actor()));
}
