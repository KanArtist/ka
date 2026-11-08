import { ChangeDetectionStrategy, Component, HostListener, computed, effect, inject, signal } from '@angular/core';
import { NavigationEnd, Router, RouterLink, RouterLinkActive, RouterOutlet } from '@angular/router';
import { Api } from './api/api';
import { Live } from './live';
import { Prefs } from './prefs';
import { Avatar } from './ui/actor';
import { Palette } from './ui/palette';
import { Peek } from './views/peek/peek';

@Component({
  selector: 'app-root',
  imports: [RouterOutlet, RouterLink, RouterLinkActive, Palette, Avatar, Peek],
  templateUrl: './app.html',
  styleUrl: './app.css',
  changeDetection: ChangeDetectionStrategy.OnPush,
})
export class App {
  protected live = inject(Live);
  protected api = inject(Api);
  protected prefs = inject(Prefs);
  private router = inject(Router);

  protected readonly palette = signal(false);
  /** The ticket in the side panel (?peek=ID), over whatever view is open. */
  protected readonly peek = signal('');
  protected readonly failed = signal('');
  private pendingG = false;
  private gTimer?: ReturnType<typeof setTimeout>;

  protected readonly agentsLive = computed(() => this.live.presence().length);
  protected readonly syncLabel = computed(() => {
    const trouble = this.live.syncTrouble();
    if (this.live.connection() !== 'live') return { cls: 'warn', text: 'Reconnecting…' };
    if (trouble.length) {
      const s = trouble[0];
      return { cls: 'warn', text: `${s.store}: ${s.state}${s.ahead ? `, ${s.ahead} to push` : ''}` };
    }
    return { cls: 'ok', text: 'Live' };
  });

  constructor() {
    const readPeek = () => {
      const p = this.router.parseUrl(this.router.url).queryParams['peek'];
      this.peek.set(typeof p === 'string' ? p.toUpperCase() : '');
    };
    readPeek(); // the initial navigation may already be done
    this.router.events.subscribe((e) => {
      if (e instanceof NavigationEnd) readPeek();
    });
    this.live.start().catch((e) => this.failed.set(e?.message ?? String(e)));
    // The tab title carries the inbox count, so it shows from another tab.
    effect(() => {
      const n = this.live.inboxCount();
      const ws = this.live.workspace();
      const name = ws?.name || ws?.projects?.[0] || 'KanArtist';
      document.title = (n ? `(${n}) ` : '') + name + ' · KanArtist';
    });
  }

  protected closePeek(): void {
    void this.router.navigate([], peekParams(null));
  }

  @HostListener('document:keydown', ['$event'])
  protected onKey(e: KeyboardEvent): void {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
      e.preventDefault();
      this.palette.update((v) => !v);
      return;
    }
    if (this.palette() || isTyping(e)) return;
    if (e.key === 'Escape' && this.peek()) {
      e.preventDefault();
      this.closePeek();
      return;
    }
    if (e.key === '/') {
      e.preventDefault();
      this.palette.set(true);
      return;
    }
    if (this.pendingG) {
      this.pendingG = false;
      const to: Record<string, string> = { a: '/activity', b: '/board', g: '/agents', i: '/inbox' };
      if (to[e.key]) {
        e.preventDefault();
        void this.router.navigate([to[e.key]]);
      }
      return;
    }
    if (e.key === 'g' && !e.metaKey && !e.ctrlKey && !e.altKey) {
      this.pendingG = true;
      clearTimeout(this.gTimer);
      this.gTimer = setTimeout(() => (this.pendingG = false), 900);
    }
  }
}

export function peekParams(id: string | null) {
  return { queryParams: { peek: id }, queryParamsHandling: 'merge' as const };
}

function isTyping(e: KeyboardEvent): boolean {
  const t = e.target as HTMLElement | null;
  return !!t && (t.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(t.tagName));
}
