import { Injectable, inject, signal } from '@angular/core';
import { Api, ApiError, Summary, WriteOut } from './api/api';
import { Live } from './live';
import { Toasts } from './ui/toast';

/** A close (completed or canceled status) waiting for its resolution and summary. */
export interface Closing {
  id: string;
  title: string;
  status: string;
  category: string;
  resolutions: string[];
  done: (r: { resolution: string; summary: string } | null) => void;
}

export interface CreateTicket {
  project: string;
  title: string;
  kind?: string;
  priority?: string;
  status?: string;
  body?: string;
  labels?: string[];
  assignee?: string;
  parent?: string;
}

/**
 * Every write the UI makes (PRD §13.3: human writes are attributed to the
 * logged-in actor and committed by the daemon like any other operation).
 * The SSE stream brings the result back into Live; failures become toasts.
 */
@Injectable({ providedIn: 'root' })
export class Actions {
  private api = inject(Api);
  private live = inject(Live);
  private toasts = inject(Toasts);

  /** The close dialog, when a transition needs a resolution. */
  readonly closing = signal<Closing | null>(null);
  /** The create dialog: open with optional defaults. */
  readonly creating = signal<Partial<CreateTicket> | null>(null);

  private path(id: string, rest = ''): string {
    return `/tickets/${encodeURIComponent(id)}${rest}`;
  }

  private async run<T>(what: string, fn: () => Promise<T>): Promise<T | null> {
    try {
      const out = await fn();
      const w = out as any;
      if (w?.push_error) this.toasts.show(`Saved locally; push failed: ${w.push_error}`, 'error', 'ka sync retries; the change is committed');
      return out;
    } catch (e) {
      this.toasts.error(e, what);
      return null;
    }
  }

  /** Set scalar fields (priority, kind, assignee, title, fields.x; null clears). */
  async set(id: string, set: Record<string, unknown>): Promise<WriteOut | null> {
    return this.run('Could not save', async () => (await this.api.patch<WriteOut>(this.path(id), { set })).body);
  }

  async addRemove(id: string, key: string, add: string[], remove: string[]): Promise<WriteOut | null> {
    const body: any = {};
    if (add.length) body.add = { [key]: add };
    if (remove.length) body.remove = { [key]: remove };
    return this.run('Could not save', async () => (await this.api.patch<WriteOut>(this.path(id), body)).body);
  }

  /**
   * Replace the description. etag is the version being edited; a conflict
   * (someone changed the ticket since) is returned, not toasted, so the
   * editor can keep the user's text.
   */
  async saveBody(id: string, body: string, etag: string): Promise<{ ok: boolean; conflict?: boolean; error?: string }> {
    try {
      await this.api.patch<WriteOut>(this.path(id), { body }, { 'If-Match': etag });
      return { ok: true };
    } catch (e) {
      if (e instanceof ApiError && (e.status === 412 || e.code === 'conflict')) return { ok: false, conflict: true };
      return { ok: false, error: (e as any)?.message ?? String(e) };
    }
  }

  /**
   * Move a ticket to a status. Closing asks for a resolution and summary
   * first; returns false if the user backed out or the move failed.
   */
  async transition(t: Pick<Summary, 'id' | 'title' | 'project' | 'status'>, status: string): Promise<boolean> {
    if (status === t.status) return true;
    const category = this.live.category({ project: t.project, status });
    let resolution = '';
    let summary = '';
    if (category === 'completed' || category === 'canceled') {
      const r = await new Promise<{ resolution: string; summary: string } | null>((done) =>
        this.closing.set({
          id: t.id,
          title: t.title,
          status,
          category,
          resolutions: this.live.schema()?.resolutions ?? ['done', 'wontfix', 'duplicate', 'obsolete'],
          done,
        }),
      );
      this.closing.set(null);
      if (!r) return false;
      ({ resolution, summary } = r);
    }
    const out = await this.run(`Could not move ${t.id}`, async () =>
      (await this.api.post<WriteOut>(this.path(t.id, '/transition'), { status, resolution: resolution || undefined, summary: summary || undefined })).body,
    );
    return !!out;
  }

  async claim(id: string, steal = false) {
    return this.run(`Could not claim ${id}`, async () => (await this.api.post(this.path(id, '/claim'), { steal })).body);
  }

  async release(id: string, force = false) {
    return this.run(`Could not release ${id}`, async () => (await this.api.post(this.path(id, '/release'), { force })).body);
  }

  async comment(id: string, body: string, type = 'note'): Promise<boolean> {
    const out = await this.run('Could not comment', async () => (await this.api.post(this.path(id, '/comments'), { body, type })).body);
    return !!out;
  }

  async check(id: string, index: number, done: boolean) {
    return this.run('Could not update acceptance', async () => (await this.api.post(this.path(id, '/acceptance'), { items: [{ index, done }] })).body);
  }

  async saveBoard(b: { store: string; key: string; title: string; description?: string; filter?: string; swimlanes?: string; group_by?: string; columns?: string[]; card_fields?: string[] }) {
    const out = await this.run('Could not save the board', async () => (await this.api.request<any>('PUT', `/boards/${encodeURIComponent(b.key)}`, b)).body);
    if (out) {
      this.toasts.show(`Saved board ${b.store}/${b.key}`);
      await this.live.refreshBoards();
    }
    return out;
  }

  async deleteBoard(store: string, key: string): Promise<boolean> {
    const out = await this.run('Could not delete the board', async () => {
      await this.api.request('DELETE', `/boards/${encodeURIComponent(key)}?store=${encodeURIComponent(store)}`);
      return true;
    });
    if (out) {
      this.toasts.show(`Deleted board ${store}/${key}`);
      await this.live.refreshBoards();
    }
    return !!out;
  }

  async create(t: CreateTicket): Promise<string | null> {
    const out = await this.run('Could not create the ticket', async () => (await this.api.post<{ created: { id: string }[] }>('/tickets', t)).body);
    const id = (out as any)?.created?.[0]?.id ?? (out as any)?.id ?? null;
    if (id) this.toasts.show(`Created ${id}`);
    return id;
  }
}
