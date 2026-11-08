import { Router } from '@angular/router';
import { Marked, type TokenizerAndRendererExtension, type Tokens } from 'marked';

/** Store paths of project docs, as tickets write them (PRD §7.2). */
const DOC_PATH = /^(?:\.\.\/)*projects\/([a-z][a-z0-9-]{0,31})\/docs\/([A-Za-z0-9_][A-Za-z0-9_./-]*[A-Za-z0-9_])/;

export function docHref(project: string, path: string): string {
  return `/docs?project=${encodeURIComponent(project)}&path=${encodeURIComponent(path)}`;
}

function esc(s: string): string {
  return s.replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]!);
}

/** Bare doc paths in text become links to the Docs view. */
const docLink: TokenizerAndRendererExtension = {
  name: 'docLink',
  level: 'inline',
  start(src: string) {
    const i = src.search(/projects\/[a-z]/);
    return i < 0 ? undefined : i;
  },
  tokenizer(src: string) {
    const m = DOC_PATH.exec(src);
    if (m) return { type: 'docLink', raw: m[0], project: m[1], path: m[2] };
    return undefined;
  },
  renderer(t) {
    const tok = t as unknown as { raw: string; project: string; path: string };
    return `<a href="${esc(docHref(tok.project, tok.path))}">${esc(tok.raw)}</a>`;
  },
};

const md = new Marked({ gfm: true, async: false });
md.use({
  extensions: [docLink],
  walkTokens(token) {
    // [text](projects/jk/docs/a.md) opens the doc in the app.
    if (token.type === 'link') {
      const l = token as Tokens.Link;
      const m = DOC_PATH.exec(l.href);
      if (m) l.href = docHref(m[1], m[2]);
    }
  },
  renderer: {
    // `projects/jk/docs/a.md` in backticks links too.
    codespan(t: Tokens.Codespan) {
      const m = DOC_PATH.exec(t.text);
      if (!m || m[0].length !== t.text.length) return false; // the default renderer
      return `<a href="${esc(docHref(m[1], m[2]))}"><code>${esc(t.text)}</code></a>`;
    },
  },
});

/** Markdown to HTML. Angular's [innerHTML] sanitizer strips scripts, handlers and unsafe URLs. */
export function renderMarkdown(src: string | undefined): string {
  if (!src?.trim()) return '';
  return md.parse(src) as string;
}

/**
 * Click handler for containers of rendered markdown: in-app links (to
 * /docs, or ?peek=) navigate with the router instead of reloading.
 */
export function followInternalLink(e: MouseEvent, router: Router): void {
  if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
  const a = (e.target as HTMLElement | null)?.closest('a');
  const href = a?.getAttribute('href');
  if (!href || !href.startsWith('/') || href.startsWith('//')) return;
  e.preventDefault();
  void router.navigateByUrl(href);
}
