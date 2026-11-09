import { ChangeDetectionStrategy, Component, computed, effect, inject, signal, untracked } from '@angular/core';
import { NavigationEnd, Router } from '@angular/router';
import { Api } from '../../api/api';
import { Live } from '../../live';
import { ProjectPipe } from '../../ui/project';

interface Entry {
  key: string;
  value: string;
  class: 'data' | 'runtime';
  source: string;
  line?: number;
  locked_by?: string;
  note?: string;
}

interface Explain {
  project: string;
  store: string;
  layers: string[];
  entries: Entry[];
  locked?: string[];
}

/**
 * Project settings (PRD §9.3, "Seeing where a value came from"): the
 * effective config of a project and the layer each value came from, with
 * locks. Read-only: config changes are ordinary commits to the stores.
 */
@Component({
  selector: 'ka-settings',
  imports: [ProjectPipe],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <section class="settings">
      <header class="head">
        <h1>Project settings</h1>
        @if (projects().length > 1) {
          <select [value]="project()" (change)="pick($any($event.target).value)" aria-label="Project">
            @for (p of projects(); track p.key) {
              <option [value]="p.key" [selected]="p.key === project()">{{ p.name || p.key }}</option>
            }
          </select>
        }
      </header>
      <p class="muted lead">
        The effective configuration of <strong [title]="project()">{{ project() | project }}</strong> and where each value comes from. To change it, edit and commit the file shown
        (<code>ka config explain --project {{ project() }}</code> prints the same).
      </p>
      @if (error()) {
        <p class="error-text">{{ error() }}</p>
      }
      @if (data(); as d) {
        <ol class="layers" aria-label="Layers, lowest first">
          @for (l of d.layers; track l; let last = $last) {
            <li [class.top]="last">{{ l }}</li>
          }
        </ol>
        <input class="field filter" type="search" placeholder="Filter keys" [value]="filter()" (input)="filter.set($any($event.target).value)" aria-label="Filter keys" />
        @for (g of groups(); track g.title) {
          <h2>{{ g.title }}</h2>
          <table>
            <thead>
              <tr><th>Key</th><th>Value</th><th>Set by</th></tr>
            </thead>
            <tbody>
              @for (e of g.entries; track e.key; let odd = $odd) {
                <tr [class.deleted]="e.note === 'deleted'" [class.stripe]="odd">
                  <td class="k">{{ e.key }}</td>
                  <td class="v">{{ e.note === 'deleted' ? 'deleted' : e.value }}</td>
                  <td class="s">
                    <span class="src" [class.builtin]="e.source === 'built-in defaults'">{{ e.source }}{{ e.line ? ':' + e.line : '' }}</span>
                    @if (e.locked_by) {
                      <span class="lock" [title]="'Locked by ' + e.locked_by + '; child layers cannot override it'">locked</span>
                    }
                    @if (e.note && e.note !== 'deleted') {
                      <span class="note">{{ e.note }}</span>
                    }
                  </td>
                </tr>
              }
            </tbody>
          </table>
        }
      }
    </section>
  `,
  styles: `
    .settings {
      padding: 18px 24px 48px;
      max-width: 1100px;
    }
    .head {
      display: flex;
      align-items: center;
      gap: 12px;
    }
    h1 {
      margin: 0;
      font-size: var(--fs-xl);
      font-weight: 650;
      color: var(--text-strong);
    }
    select {
      height: 26px;
      border: 1px solid var(--line);
      border-radius: var(--r-md);
      background: var(--surface);
      padding: 0 8px;
      font-size: var(--fs-sm);
    }
    .lead {
      max-width: 75ch;
    }
    .layers {
      display: flex;
      flex-wrap: wrap;
      gap: 6px;
      list-style: none;
      padding: 0;
      margin: 12px 0 14px;
      font: 400 var(--fs-xs) var(--mono);
    }
    .layers li {
      padding: 3px 8px;
      border-radius: var(--r-md);
      background: var(--surface);
      border: 1px solid var(--line-soft);
      color: var(--muted);
    }
    .layers li:not(:last-child)::after {
      content: ' →';
      color: var(--faint);
    }
    .layers li.top {
      color: var(--text-strong);
      border-color: var(--line);
    }
    .filter {
      max-width: 280px;
      height: 28px;
      padding: 4px 8px;
      font-size: var(--fs-sm);
    }
    h2 {
      margin: 22px 0 6px;
      font-size: var(--fs-lg);
      font-weight: 650;
      color: var(--text-strong);
    }
    table {
      width: 100%;
      border-collapse: collapse;
      font-size: var(--fs-sm);
      table-layout: fixed;
    }
    th:nth-child(1) {
      width: 260px;
    }
    th:nth-child(3) {
      width: 300px;
    }
    th {
      text-align: left;
      color: var(--muted);
      font-weight: 600;
      padding: 4px 8px;
      border-bottom: 1px solid var(--line);
    }
    tbody tr:hover {
      background: var(--row-hover);
    }
    td {
      padding: 5px 8px;
      border-bottom: 1px solid var(--line-soft);
      vertical-align: top;
    }
    .k {
      font-family: var(--mono);
      color: var(--text-strong);
      overflow-wrap: anywhere;
    }
    .v {
      color: var(--text);
      overflow-wrap: anywhere;
    }
    .deleted .v {
      color: var(--faint);
      text-decoration: line-through;
    }
    .s {
      overflow-wrap: anywhere;
    }
    .src {
      font-family: var(--mono);
      font-size: var(--fs-xs);
      color: var(--text);
    }
    .src.builtin {
      color: var(--faint);
    }
    .lock,
    .note {
      margin-left: 6px;
      font-size: var(--fs-xs);
      padding: 0 6px;
      border-radius: 9px;
    }
    .lock {
      color: var(--ink-human);
      background: var(--ink-human-wash);
    }
    .note {
      color: var(--muted);
      background: var(--raised);
    }
  `,
})
export class Settings {
  private api = inject(Api);
  private router = inject(Router);
  private live = inject(Live);

  protected readonly projects = computed(() => this.live.schema()?.projects ?? []);
  protected readonly project = signal('');
  protected readonly data = signal<Explain | null>(null);
  protected readonly error = signal('');
  protected readonly filter = signal('');

  protected readonly groups = computed(() => {
    const d = this.data();
    if (!d) return [];
    const q = this.filter().trim().toLowerCase();
    const es = d.entries.filter((e) => !q || e.key.toLowerCase().includes(q) || e.value.toLowerCase().includes(q));
    return [
      { title: 'Data (validation; resolved through extends)', entries: es.filter((e) => e.class === 'data') },
      { title: 'Runtime (this ka; your workspace wins)', entries: es.filter((e) => e.class === 'runtime') },
    ].filter((g) => g.entries.length);
  });

  constructor() {
    const read = () => {
      const p = this.router.parseUrl(this.router.url).queryParams['project'];
      const fallback = this.live.schema()?.default_project || this.projects()[0]?.key || '';
      this.project.set(typeof p === 'string' && p ? p : fallback);
    };
    read();
    this.router.events.subscribe((e) => {
      if (e instanceof NavigationEnd) read();
    });
    effect(() => {
      const p = this.project();
      untracked(() => void this.load(p));
    });
  }

  protected pick(key: string): void {
    void this.router.navigate([], { queryParams: { project: key }, queryParamsHandling: 'merge' });
  }

  private async load(project: string): Promise<void> {
    this.data.set(null);
    if (!project) return;
    try {
      const r = await this.api.get<Explain>(`/projects/${encodeURIComponent(project)}/config`);
      this.data.set(r.body);
      this.error.set('');
    } catch (e: any) {
      this.error.set(e?.message ?? String(e));
    }
  }
}
