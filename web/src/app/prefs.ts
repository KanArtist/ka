import { Injectable, effect, signal } from '@angular/core';

type Theme = 'dark' | 'light';
type Density = 'compact' | 'comfortable';

function stored<T extends string>(key: string, allowed: readonly T[], fallback: T): T {
  const v = localStorage.getItem(key) as T | null;
  return v && allowed.includes(v) ? v : fallback;
}

/** Theme and density, remembered in this browser. Dark and compact by default. */
@Injectable({ providedIn: 'root' })
export class Prefs {
  readonly theme = signal<Theme>(stored('ka.theme', ['dark', 'light'] as const, 'dark'));
  readonly density = signal<Density>(stored('ka.density', ['compact', 'comfortable'] as const, 'compact'));

  constructor() {
    effect(() => {
      document.documentElement.dataset['theme'] = this.theme();
      localStorage.setItem('ka.theme', this.theme());
    });
    effect(() => {
      document.documentElement.dataset['density'] = this.density();
      localStorage.setItem('ka.density', this.density());
    });
  }

  toggleTheme(): void {
    this.theme.update((t) => (t === 'dark' ? 'light' : 'dark'));
  }

  toggleDensity(): void {
    this.density.update((d) => (d === 'compact' ? 'comfortable' : 'compact'));
  }
}
