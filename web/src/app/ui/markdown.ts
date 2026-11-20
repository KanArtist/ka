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

/** Ticket IDs (spec §3.1) standing alone in text open in the side panel. */
const TICKET_ID = /^[A-Z][A-Z0-9]{0,9}-[0-9A-HJKMNP-TV-Z]{5,8}(?![A-Za-z0-9_-])/;

/** Whether s is a whole ticket ID (spec §3.1). */
export function isTicketId(s: string): boolean {
  return /^[A-Z][A-Z0-9]{0,9}-[0-9A-HJKMNP-TV-Z]{5,8}$/.test(s);
}
const ticketLink: TokenizerAndRendererExtension = {
  name: 'ticketLink',
  level: 'inline',
  start(src: string) {
    const m = /(?<![A-Za-z0-9_\-/.#])[A-Z][A-Z0-9]{0,9}-[0-9A-HJKMNP-TV-Z]{5,8}(?![A-Za-z0-9_-])/.exec(src);
    return m ? m.index : undefined;
  },
  tokenizer(this: { lexer: { state: { inLink: boolean } } }, src: string) {
    if (this.lexer.state.inLink) return undefined;
    const m = TICKET_ID.exec(src);
    if (m) return { type: 'ticketLink', raw: m[0] };
    return undefined;
  },
  renderer(t) {
    const id = (t as unknown as { raw: string }).raw;
    return `<a class="ticket-ref" href="?peek=${esc(id)}">${esc(id)}</a>`;
  },
};

/** Link schemes that render as links; others (vscode:, ssh:, javascript:) render as their text. */
const SAFE_SCHEMES = new Set(['http', 'https', 'mailto']);

const md = new Marked({ gfm: true, async: false });
md.use({
  extensions: [docLink, ticketLink],
  walkTokens(token) {
    // [text](projects/jk/docs/a.md) opens the doc in the app.
    if (token.type === 'link') {
      const l = token as Tokens.Link;
      const m = DOC_PATH.exec(l.href);
      if (m) l.href = docHref(m[1], m[2]);
    }
  },
  renderer: {
    // Ticket text is written by agents and others: raw HTML shows as text,
    // so it cannot dress up as the app's own buttons or forms. HTML comments
    // (ticket templates use them for hints) stay hidden.
    html(t: Tokens.HTML | Tokens.Tag) {
      return esc(t.text.replace(/<!--[\s\S]*?-->/g, ''));
    },
    // Only known-safe schemes link; web links open in a new tab, without
    // the opener or the referrer. Relative, #hash and in-app links render as usual.
    link(t: Tokens.Link) {
      const href = t.href.trim();
      const scheme = /^([a-z][a-z0-9+.-]*):/i.exec(href)?.[1]?.toLowerCase();
      if (scheme && !SAFE_SCHEMES.has(scheme)) return this.parser.parseInline(t.tokens);
      if (scheme !== 'http' && scheme !== 'https') return false; // the default renderer
      const title = t.title ? ` title="${esc(t.title)}"` : '';
      return `<a href="${esc(href)}"${title} target="_blank" rel="noopener noreferrer">${this.parser.parseInline(t.tokens)}</a>`;
    },
    // `projects/jk/docs/a.md` in backticks links too.
    codespan(t: Tokens.Codespan) {
      const m = DOC_PATH.exec(t.text);
      if (!m || m[0].length !== t.text.length) return false; // the default renderer
      return `<a href="${esc(docHref(m[1], m[2]))}"><code>${esc(t.text)}</code></a>`;
    },
  },
});

/**
 * Markdown to HTML, with raw HTML escaped and link schemes allowlisted.
 * Angular's [innerHTML] sanitizer still strips scripts, handlers and unsafe URLs.
 */
export function renderMarkdown(src: string | undefined): string {
  if (!src?.trim()) return '';
  return md.parse(src) as string;
}

/**
 * Click handler for containers of rendered markdown: in-app links (to
 * /docs, or ?peek=ID from ticket references) navigate with the router instead of reloading.
 */
export function followInternalLink(e: MouseEvent, router: Router): void {
  if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
  const a = (e.target as HTMLElement | null)?.closest('a');
  const href = a?.getAttribute('href');
  if (href?.startsWith('?peek=')) {
    e.preventDefault();
    const id = (new URLSearchParams(href).get('peek') ?? '').toUpperCase();
    if (isTicketId(id)) void router.navigate([], { queryParams: { peek: id }, queryParamsHandling: 'merge' });
    return;
  }
  if (!href || !href.startsWith('/') || href.startsWith('//')) return;
  e.preventDefault();
  void router.navigateByUrl(href);
}
