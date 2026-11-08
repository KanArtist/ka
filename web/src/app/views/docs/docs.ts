import { ChangeDetectionStrategy, Component, computed, effect, inject, signal, untracked } from '@angular/core';
import { NavigationEnd, Router, RouterLink } from '@angular/router';
import { Api, ApiError } from '../../api/api';
import { Live } from '../../live';
import { followInternalLink, renderMarkdown } from '../../ui/markdown';

interface DocEntry {
  project: string;
  path: string;
  title: string;
  size: number;
}

interface DocOut extends DocEntry {
  store: string;
  store_path: string;
  body: string;
  text: boolean;
  truncated?: boolean;
  backlinks: { id: string; title?: string; status?: string }[];
}

/**
 * Project docs (PRD §13.2): browse and render projects/<key>/docs/, with
 * the tickets that mention each doc. Deep link: /docs?project=jk&path=a.md.
 */
@Component({
  selector: 'ka-docs',
  imports: [RouterLink],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="docs">
      <nav class="list" aria-label="Documents">
        <input class="field filter" type="search" placeholder="Filter documents" [value]="filter()" (input)="filter.set($any($event.target).value)" aria-label="Filter documents" />
        @for (g of groups(); track g.project) {
          <h2>{{ g.project }} <span class="n">{{ g.docs.length }}</span></h2>
          <ul>
            @for (d of g.docs; track d.path) {
              <li>
                <a [routerLink]="[]" [queryParams]="{ project: d.project, path: d.path }" [class.on]="d.project === project() && d.path === path()" [title]="d.path">
                  @if (dir(d.path); as dd) {
                    <span class="dir">{{ dd }}/</span>
                  }
                  <span class="t">{{ d.title }}</span>
                </a>
              </li>
            }
          </ul>
        } @empty {
          @if (loaded()) {
            <p class="muted pad">No documents. A project's docs live in <code>projects/&lt;key&gt;/docs/</code>.</p>
          }
        }
      </nav>
      <article class="doc" (click)="linkClick($event)">
        @if (error(); as e) {
          <p class="muted">{{ e }}</p>
        } @else if (doc(); as d) {
          <header>
            <p class="path"><span class="id">{{ d.store }}</span> {{ d.store_path }}</p>
          </header>
          @if (!d.text) {
            <p class="muted">This file is not text, so it is not shown here.</p>
          } @else if (isMarkdown(d.path)) {
            <div class="md" [innerHTML]="html()"></div>
          } @else {
            <pre class="plain">{{ d.body }}</pre>
          }
          @if (d.truncated) {
            <p class="muted">Only the first megabyte is shown.</p>
          }
          <section class="backlinks">
            <h2>Mentioned by <span class="n">{{ d.backlinks.length }}</span></h2>
            @for (b of d.backlinks; track b.id) {
              <a class="rel" [routerLink]="[]" [queryParams]="{ peek: b.id }" queryParamsHandling="merge">
                <span class="dot" [attr.data-cat]="relCat(b.id)"></span><span class="id">{{ b.id }}</span>{{ b.title }}
              </a>
            } @empty {
              <p class="muted">No ticket links this document. Link it from a ticket as <code>{{ d.store_path }}</code>.</p>
            }
          </section>
        } @else if (project()) {
          <p class="muted">Loading…</p>
        } @else {
          <p class="muted hint">Pick a document. Tickets link docs by their store path, such as <code>projects/jk/docs/design.md</code>; the links open here.</p>
        }
      </article>
    </div>
  `,
  styles: `
    .docs {
      display: grid;
      grid-template-columns: 300px minmax(0, 1fr);
      height: 100%;
      min-height: 0;
    }
    .list {
      border-right: 1px solid var(--line-soft);
      overflow: auto;
      padding: 14px 10px 30px 16px;
    }
    .filter {
      margin-bottom: 8px;
      height: 28px;
      padding: 4px 8px;
      font-size: var(--fs-sm);
    }
    h2 {
      margin: 14px 0 4px;
      font-size: var(--fs-sm);
      font-weight: 650;
      color: var(--text-strong);
    }
    .n {
      color: var(--faint);
      font-weight: 500;
    }
    ul {
      list-style: none;
      margin: 0;
      padding: 0;
    }
    .list a {
      display: block;
      padding: 3px 8px;
      border-radius: var(--r-md);
      text-decoration: none;
      color: var(--text);
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }
    .list a:hover {
      background: var(--raised);
    }
    .list a.on {
      background: var(--raised);
      color: var(--text-strong);
      box-shadow: inset 2px 0 0 var(--ink-human);
    }
    .dir {
      color: var(--faint);
      font-size: var(--fs-sm);
    }
    .pad {
      padding: 8px;
    }
    .doc {
      overflow: auto;
      padding: 18px 32px 60px;
    }
    .path {
      margin: 0 0 18px;
      color: var(--muted);
      font: 400 var(--fs-sm) var(--mono);
    }
    .plain {
      font: 400 var(--fs-sm) / 1.55 var(--mono);
      white-space: pre-wrap;
    }
    .backlinks {
      margin-top: 36px;
      padding-top: 14px;
      border-top: 1px solid var(--line-soft);
      max-width: 72ch;
    }
    .rel {
      display: flex;
      align-items: center;
      gap: 8px;
      line-height: 26px;
      text-decoration: none;
      color: var(--text);
    }
    .rel:hover {
      color: var(--text-strong);
    }
    .dot {
      width: 8px;
      height: 8px;
      border-radius: 50%;
      background: var(--cat, var(--faint));
      flex: none;
    }
    .hint {
      max-width: 60ch;
    }
  `,
})
export class Docs {
  private api = inject(Api);
  private router = inject(Router);
  private live = inject(Live);

  protected readonly all = signal<DocEntry[]>([]);
  protected readonly loaded = signal(false);
  protected readonly filter = signal('');
  protected readonly project = signal('');
  protected readonly path = signal('');
  protected readonly doc = signal<DocOut | null>(null);
  protected readonly error = signal('');

  protected readonly groups = computed(() => {
    const q = this.filter().trim().toLowerCase();
    const m = new Map<string, DocEntry[]>();
    for (const d of this.all()) {
      if (q && !d.title.toLowerCase().includes(q) && !d.path.toLowerCase().includes(q)) continue;
      m.set(d.project, [...(m.get(d.project) ?? []), d]);
    }
    return [...m.entries()].map(([project, docs]) => ({ project, docs }));
  });

  protected readonly html = computed(() => renderMarkdown(this.doc()?.body));

  constructor() {
    void this.loadList();
    const read = () => {
      const qp = this.router.parseUrl(this.router.url).queryParams;
      this.project.set(typeof qp['project'] === 'string' ? qp['project'] : '');
      this.path.set(typeof qp['path'] === 'string' ? qp['path'] : '');
    };
    read();
    this.router.events.subscribe((e) => {
      if (e instanceof NavigationEnd) read();
    });
    effect(() => {
      const p = this.project();
      const path = this.path();
      untracked(() => void this.loadDoc(p, path));
    });
  }

  private async loadList(): Promise<void> {
    try {
      this.all.set((await this.api.get<{ docs: DocEntry[] }>('/docs')).body.docs ?? []);
    } catch (e: any) {
      this.error.set(e?.message ?? String(e));
    }
    this.loaded.set(true);
  }

  private async loadDoc(project: string, path: string): Promise<void> {
    this.doc.set(null);
    this.error.set('');
    if (!project || !path) return;
    try {
      const r = await this.api.get<DocOut>(`/projects/${encodeURIComponent(project)}/doc?path=${encodeURIComponent(path)}`);
      if (project === this.project() && path === this.path()) this.doc.set(r.body);
    } catch (e) {
      this.error.set(e instanceof ApiError ? e.message + (e.fix ? ` (${e.fix})` : '') : String(e));
    }
  }

  protected linkClick(e: MouseEvent): void {
    followInternalLink(e, this.router);
  }

  protected dir(p: string): string {
    const i = p.lastIndexOf('/');
    return i < 0 ? '' : p.slice(0, i);
  }

  protected isMarkdown(p: string): boolean {
    return /\.(md|markdown)$/i.test(p);
  }

  protected relCat(id: string): string {
    const t = this.live.tickets().get(id);
    return t ? this.live.category(t) : '';
  }
}
