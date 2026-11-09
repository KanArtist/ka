import { Pipe, PipeTransform } from '@angular/core';

/*
 * Labels people read. Statuses, kinds, field names and the like are spec
 * identifiers ("in_progress", "legacy_id"); the UI shows them as words and
 * keeps the identifier for queries, the API and files (and for tooltips).
 */

/** Words that read as acronyms. */
const ACRONYMS = new Set(['id', 'ids', 'url', 'urls', 'api', 'pr', 'prs', 'ci', 'cd', 'ui', 'ux', 'mcp', 'qa', 'sla', 'eta', 'db', 'os', 'ssh', 'http', 'https', 'json', 'yaml', 'cli', 'sdk']);

/** "in_progress" is "In Progress"; "legacy_id" is "Legacy ID"; "won't-fix" is "Won't Fix". */
export function humanize(s: string | undefined | null): string {
  if (!s) return '';
  return s
    .split(/[_\-\s]+/)
    .filter(Boolean)
    .map((w) => (ACRONYMS.has(w.toLowerCase()) ? w.toUpperCase() : w[0].toUpperCase() + w.slice(1)))
    .join(' ');
}

/** A status: "in_progress" is "In Progress". */
export const statusLabel = humanize;

/** A ticket kind: "feature" is "Feature". */
export const kindLabel = humanize;

/** A custom field's name: "fields.legacy_id" and "legacy_id" are both "Legacy ID". */
export function fieldLabel(key: string | undefined | null): string {
  return humanize((key ?? '').replace(/^fields\./, ''));
}

const CATEGORIES: Record<string, string> = {
  triage: 'Triage',
  backlog: 'Backlog',
  unstarted: 'Not started',
  started: 'In progress',
  completed: 'Done',
  canceled: 'Canceled',
};

/** A status category, which groups statuses across projects. */
export function categoryLabel(c: string | undefined | null): string {
  return (c && CATEGORIES[c]) || humanize(c);
}

@Pipe({ name: 'status' })
export class StatusPipe implements PipeTransform {
  transform(s: string | undefined | null): string {
    return statusLabel(s);
  }
}

@Pipe({ name: 'kind' })
export class KindPipe implements PipeTransform {
  transform(s: string | undefined | null): string {
    return kindLabel(s);
  }
}

@Pipe({ name: 'field' })
export class FieldPipe implements PipeTransform {
  transform(s: string | undefined | null): string {
    return fieldLabel(s);
  }
}

/** {{ code | words }}: any identifier as words ("store_unavailable" is "Store Unavailable"). */
@Pipe({ name: 'words' })
export class WordsPipe implements PipeTransform {
  transform(s: string | undefined | null): string {
    return humanize(s);
  }
}

const RESOLUTIONS: Record<string, string> = { wontfix: "Won't fix", wont_fix: "Won't fix", done: 'Done', duplicate: 'Duplicate', obsolete: 'Obsolete' };

/** How a ticket was closed: "wontfix" is "Won't fix". */
export function resolutionLabel(r: string | undefined | null): string {
  return (r && RESOLUTIONS[r]) || humanize(r);
}

@Pipe({ name: 'resolution' })
export class ResolutionPipe implements PipeTransform {
  transform(s: string | undefined | null): string {
    return resolutionLabel(s);
  }
}
