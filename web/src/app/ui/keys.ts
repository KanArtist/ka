import { Pipe, PipeTransform } from '@angular/core';

/** Whether shortcuts use ⌘ (macOS, iOS) rather than Ctrl. */
export const IS_MAC = ((): boolean => {
  if (typeof navigator === 'undefined') return false;
  const p = (navigator as Navigator & { userAgentData?: { platform?: string } }).userAgentData?.platform || navigator.platform || navigator.userAgent;
  return /mac|iphone|ipad|ipod/i.test(p);
})();

/**
 * A shortcut written for this platform. "Mod" is ⌘ on a Mac and Ctrl
 * elsewhere; "Enter" is ↵ on a Mac. keys('Mod+Enter') is "⌘↵" on a Mac and
 * "Ctrl+Enter" on Windows and Linux.
 */
export function keys(combo: string): string {
  if (!IS_MAC) return combo.replace(/\bMod\b/g, 'Ctrl');
  return combo
    .replace(/\bMod\+/g, '⌘')
    .replace(/\bShift\+/g, '⇧')
    .replace(/\bAlt\+/g, '⌥')
    .replace(/\bEnter\b/g, '↵');
}

@Pipe({ name: 'keys' })
export class KeysPipe implements PipeTransform {
  transform(combo: string): string {
    return keys(combo);
  }
}
