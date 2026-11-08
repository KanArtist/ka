import { Injectable, signal } from '@angular/core';
import type { components } from './schema';

type S = components['schemas'];
export type Summary = S['Summary'];
export type Detail = S['Detail'];
export type WorkspaceOut = S['WorkspaceOut'];
export type StoreOut = S['StoreOut'];
export type SyncState = S['SyncState'];
export type InboxOut = S['InboxOut'];
export type InboxItem = S['InboxItem'];
export type CommentOut = S['CommentOut'];
export type SearchOut = S['SearchOut'];
export type SchemaOut = S['SchemaOut'];
export type WriteOut = S['WriteOut'];
export type Problem = S['Problem'];

/** A live presence session (GET /presence, presence.changed). */
export interface Session {
  id: string;
  actor: string;
  on_behalf_of?: string;
  client?: string;
  transport: string;
  connected: string;
  last_seen: string;
  last_method?: string;
  last_tool?: string;
  last_ticket?: string;
  last_error?: string;
  calls: number;
}

/** A domain event from GET /api/v1/events. */
export interface KaEvent {
  id: number;
  type: string;
  at: string;
  store?: string;
  project?: string;
  ticket?: string;
  actor?: string;
  data?: any;
}

/** An API error, carrying the engine's code and fix. */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly problem: Partial<Problem>,
  ) {
    super(problem.detail || problem.title || `HTTP ${status}`);
  }
  get code(): string {
    return this.problem.code ?? '';
  }
  get fix(): string {
    return this.problem.fix ?? '';
  }
}

export interface ApiResult<T> {
  body: T;
  seq: number; // Ka-Event-Seq at the time of the response
  etag?: string;
}

/**
 * Talks to the daemon's REST API on the same origin. The browser holds the
 * token as an HttpOnly cookie (set by ka open), so requests carry no secrets.
 */
@Injectable({ providedIn: 'root' })
export class Api {
  /** Set when the daemon answers 401: the page was not opened through ka open. */
  readonly unauthorized = signal(false);

  async request<T>(method: string, path: string, body?: unknown, headers: Record<string, string> = {}): Promise<ApiResult<T>> {
    const init: RequestInit = { method, headers: { Accept: 'application/json', ...headers }, credentials: 'same-origin' };
    if (body !== undefined) {
      init.body = JSON.stringify(body);
      (init.headers as Record<string, string>)['Content-Type'] = 'application/json';
    }
    const res = await fetch('/api/v1' + path, init);
    const seq = Number(res.headers.get('Ka-Event-Seq') ?? 0);
    const text = await res.text();
    const json = text ? JSON.parse(text) : undefined;
    if (!res.ok) {
      if (res.status === 401) this.unauthorized.set(true);
      throw new ApiError(res.status, json ?? {});
    }
    return { body: json as T, seq, etag: res.headers.get('ETag') ?? undefined };
  }

  get<T>(path: string) {
    return this.request<T>('GET', path);
  }
  post<T>(path: string, body: unknown = {}) {
    return this.request<T>('POST', path, body);
  }
  patch<T>(path: string, body: unknown, headers?: Record<string, string>) {
    return this.request<T>('PATCH', path, body, headers);
  }
}
