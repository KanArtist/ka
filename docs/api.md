# The daemon and REST API (M2)

`ka daemon` serves one workspace over HTTP: REST under `/api/v1`, described by an OpenAPI 3.1
document. It is the same `engine.Service` that `ka mcp` and the CLI use, so behavior cannot
diverge (PRD §12.1). Live changes stream from `GET /api/v1/events` (Server-Sent Events), and MCP
is served over Streamable HTTP at `/mcp`, with live presence for every agent session. The web UI
builds on it (see the M2 tickets).

## Running it

```bash
ka daemon                  # foreground; 127.0.0.1:7711, or a free port if 7711 is taken
ka daemon --listen 127.0.0.1:7800 -v    # another port; log every request
ka daemon status           # where it runs (exit 1 if none)
ka daemon stop
ka daemon openapi > openapi.json        # the OpenAPI document, without a workspace
```

The daemon finds its workspace like every other `ka` command (`--workspace`, `.ka-workspace.yml`,
inside a store, `.ka.yml`, the user workspace). Only one daemon runs per workspace; a second
`ka daemon` says where the first one is.

**The runtime file.** On start, the daemon writes `daemon-<hash>.json`, readable only by you, to
`$XDG_RUNTIME_DIR/ka/` on Linux and the user cache directory's `ka/run/` elsewhere
(`~/Library/Caches/ka/run`, `%LocalAppData%\ka\run`; `$KA_RUNTIME_DIR` overrides). `<hash>`
identifies the workspace by its source path, so daemons for different workspaces can run side by
side. The file holds the version, PID, address, URL, workspace, and the random local token.
Clients read it, then confirm with `GET /healthz` that the daemon is alive and serves the same
workspace. The daemon removes the file when it stops.

## The web UI and `ka open`

`ka open` opens the web UI in your browser, starting the workspace's daemon in the background if it
is not running. Its log goes next to the runtime file. `ka open JK-7QK3M` goes straight to a
ticket, and `--print` prints the URL instead of opening a browser. The browser lands on `/auth`,
which turns the token into a cookie.

The UI is an Angular app in `web/`. Its production build is embedded in the binary, so the daemon
serves it offline with no CDN. The UI's static files need no token, because they hold no workspace
data; without the cookie, the app tells you to run `ka open`. Every UI page is sent with a strict
Content-Security-Policy: scripts only from the daemon, and styles only from the daemon or with a
per-response nonce. Its API types are generated from `openapi.json`.

## Security (PRD §15, local mode)

- The daemon listens on loopback only. Non-loopback addresses are refused until remote mode (M4).
- Every API request (`/api/v1/…`, `/mcp`) needs the token: `Authorization: Bearer
  <token>`, or the `ka_token` cookie.
- `GET /auth?token=…&next=/path` exchanges the token for an `HttpOnly`, `SameSite=Strict` cookie
  and redirects to a local path. `ka open` will use it.
- The `Host` header must be `127.0.0.1`, `localhost` or `[::1]` with the daemon's port (DNS
  rebinding). A request with any other `Origin`, or with `Sec-Fetch-Site: cross-site`, is refused
  (CSRF).

## Conventions

- **Who is acting.** Requests act as the daemon's human (your git identity, mapped through the
  actor registry). `X-Ka-Actor: agent/<name>` acts as an agent on the human's behalf, and
  `X-Ka-Session` sets the session. Both land in the commit trailers, as they do from MCP.
- **Errors** are `application/problem+json` with the engine's `code` and `fix`:

  ```json
  {"status": 422, "title": "Unprocessable Entity", "code": "unknown_status",
   "detail": "\"doing\" is not a status", "fix": "did you mean in_progress?"}
  ```

  | HTTP | Codes |
  |---|---|
  | 401, 403 | `unauthorized`, `forbidden`, `read_only_store` |
  | 404 | `not_found`, `unknown_project`, `no_candidates`, `no_open_question` |
  | 409 | `claim_lost`, `not_claimed`, `not_your_claim`, `ticket_moved`, `id_taken` |
  | 412, 428 | `edit_conflict` (stale `If-Match`), `if_match_required` |
  | 422 | `invalid_argument`, `unknown_status`, `unknown_kind`, `unknown_label`, `unknown_field`, `resolution_required`, … |
  | 429 | `progress_throttled` |
  | 503, 502 | `git_busy`, `git_error` |

- **Lost-update protection.** `GET /tickets/{id}` and `GET /tickets/{id}/source` return an `ETag`:
  the git blob SHA of `ticket.md`. Replacing the body (`PATCH` with `body`, or `PUT …/source`)
  requires `If-Match`. The check runs under the store's write lock, against the file on disk.
  Field patches need no precondition, because they replay safely.
- Batches (`POST /tickets/batch`, `PATCH /tickets`) make one commit per ticket and push once, like
  `ka_create` and `ka_update`. A partial failure returns the successful results and the first
  `error`.

## Endpoints

| Method and path | Service | MCP / CLI |
|---|---|---|
| `GET /workspace` | WorkspaceInfo | `ka workspace` |
| `GET /stores`, `GET /sync` | Stores | `ka store ls` |
| `POST /sync` | Sync | `ka_sync` |
| `POST /hooks/git?store=` | Kick (fetch now) | forge webhooks |
| `GET /schema`, `GET /projects`, `GET /projects/{key}` | Schema | `ka_schema` |
| `GET /boards`, `GET /boards/{name}` | Boards, BoardFilter + Search | `ka://board/{name}` |
| `GET /actors` | Actors | |
| `GET /tickets?q=…`, `GET /search?q=…` | Search | `ka_search`, `ka ls` |
| `POST /tickets`, `POST /tickets/batch` | Create | `ka_create`, `ka new` |
| `GET /tickets/{id}` | Get | `ka_get`, `ka show` |
| `PATCH /tickets/{id}`, `PATCH /tickets` | Update | `ka_update`, `ka edit --set` |
| `GET`/`PUT /tickets/{id}/source` | ReplaceTicket | `ka edit` ($EDITOR) |
| `POST /tickets/{id}/claim` | Claim | `ka_claim` |
| `POST /tickets/{id}/release` | Release | `ka_release` |
| `POST /tickets/{id}/transition` | Transition | `ka_transition`, `ka move` |
| `GET`/`POST /tickets/{id}/comments` | Get, Comment | `ka_comment` |
| `POST /tickets/{id}/acceptance` | CheckItems | `ka_check` |
| `POST /tickets/{id}/questions` | Ask (`wait_seconds` ≤ 300) | `ka_ask` |
| `POST /tickets/{id}/answers` | Answer | `ka answer`, `ka approve` |
| `GET /inbox` | Inbox | `ka_inbox` |
| `POST /next` | Next | `ka_next` |
| `GET /links/resolve?link=…` | ResolveLink | `ka_resolve_link` |
| `GET /check`, `POST /fix` | Validate, Fix | `ka check`, `ka fix` |
| `GET /events` (SSE) | SetEvents, WatchLocal | |
| `GET /presence` | (daemon only) | |
| `GET /daemon`, `POST /daemon/shutdown` | | `ka daemon status`, `stop` |
| `GET /openapi.json` | | `ka daemon openapi` |

A test (`TestEveryServiceMethodHasAnEndpoint`) fails when a new exported `engine.Service` method
has no endpoint. Another validates `openapi.json` against the official OpenAPI 3.1 schema.

## MCP over HTTP (`/mcp`)

The daemon serves the same MCP tools, resources and prompts as `ka mcp`, over Streamable HTTP at
`/mcp`. It needs the bearer token. An HTTP client can connect directly:

```bash
claude mcp add --transport http ka http://127.0.0.1:7711/mcp --header "Authorization: Bearer $TOKEN"
```

Usually you don't need to: **`ka mcp` finds a running daemon for its workspace and proxies to
it.** It bridges stdio to `/mcp` and passes messages through unchanged, so the client's
`initialize` reaches the daemon and the actor is still `agent/<clientInfo.name>`. Every agent then
shares one index, its events and (soon) presence. `--as` / `$KA_ACTOR` and `$KA_SESSION` travel as
`X-Ka-Actor` and `X-Ka-Session` headers. With no daemon, or with `ka mcp --no-daemon`
(`$KA_NO_DAEMON`), the engine runs in-process, as in M1. `--offline`, `--no-push` and `--map`
also force in-process mode, because they change how the engine behaves. The MCP scenario tests
and token budgets run both ways (`TestScenariosViaDaemon`).

## Fetching

The daemon fetches every store with a remote every `fetch_interval` (15 s by default; see
[engine.md](engine.md#sync-and-replay)) and integrates what it finds, so pushes from elsewhere
arrive as events within one interval. `POST /api/v1/hooks/git` (optionally `?store=<name>`) makes
it fetch at once. Point a forge's push webhook at it in remote mode (M4), or call it from a client
that wants fresh data. In tests, a change pushed elsewhere reaches SSE clients about 25 ms after
the hook.

## Presence (`GET /api/v1/presence`)

The daemon tracks every MCP session connected to it, directly or through the `ka mcp` proxy. For
each one it records the actor, the human it works for, the client (`clientInfo` name and version),
when it connected and was last seen, its last method and tool call, the ticket that call was
about (for `ka_next`, the ticket it got), the error code if the call failed, and a call count.

Changes stream as `presence.changed` events with `state` `connected`, `active` or `disconnected`.
A session leaves presence when it closes, or when the daemon's 30-second keep-alive pings stop
reaching it; one idle for 2 hours is swept as a last resort. Presence is live state only: it is
never written to git, and it starts empty when the daemon restarts. Sessions of a `ka mcp
--no-daemon` don't appear, because they never talk to the daemon.

## Events (`GET /api/v1/events`)

The daemon turns every change into a domain event (PRD §12.2), whatever caused it:

- writes through the daemon;
- commits by other processes sharing the clone (the CLI, another `ka mcp`), which the daemon
  notices within 250 ms by watching `.git/logs/HEAD`;
- upstream changes brought in by sync.

The index diffs each reloaded ticket, before and after, so all three paths produce the same
events. Payloads carry the ticket's new summary, so a client can update itself without
refetching.

| Event | Data |
|---|---|
| `ticket.created`, `ticket.removed` | `{ticket}` |
| `ticket.updated` | `{ticket, changes: {field: {from, to}}, body_changed}` |
| `ticket.moved` (status changed) | the same, plus `from`, `to`, `category` |
| `claim.changed` | `{ticket, from, to}` (claims) |
| `comment.created` | `{ticket: {id, title, status}, comment}` |
| `question.opened`, `question.answered` | the same as `comment.created`, sent in addition to it |
| `sync.state` | a store's `SyncState`, when it changes and after every sync |
| `sync.conflict_preserved` | `{preserved: [paths]}` |
| `store.changed` | config, projects or boards changed; refetch the schema |
| `reset` | refetch your snapshot |
| `presence.changed` | `{state, session}` (see Presence) |

Every event has `id`, `type`, `at`, and, where they apply, `store`, `project`, `ticket` and
`actor` (who caused it, when known: the operation's actor, a single committer's `Ka-Actor`, a
comment's author or a claim's holder).

```
id: 1794105449411991
event: comment.created
data: {"id":1794105449411991,"type":"comment.created","ticket":"KA-ZXYN2","actor":"bsant",...}
```

- **IDs** are one monotonic sequence per daemon. They start at the daemon's start time in
  microseconds, so IDs from an earlier daemon are always older than anything buffered.
- **Replay.** The daemon keeps the last 10,000 events. Reconnect with `Last-Event-ID` (or
  `?last_event_id=`) to get everything after that ID. If the ID is no longer buffered, the stream
  starts with `reset`. Without an ID, the stream starts at the present.
- **Snapshot, then subscribe.** Every API response carries `Ka-Event-Seq`, the last event ID at
  that moment. Fetch a snapshot, then subscribe with `Last-Event-ID` set to its `Ka-Event-Seq`:
  nothing is missed or duplicated.
- **Filters:** `?types=ticket.*,comment.created&projects=jk,gyggly&tickets=JK-7QK3M`.
- **Keep-alive:** a `: ping` comment every 15 seconds.

## Measured

- On the migrated `jk` store (3,648 tickets), warm `GET /tickets?q=…` requests take about 1 ms end
  to end over loopback, against the PRD's 20 ms target.
- An operation through the daemon reaches an SSE client in about 45 ms, including the commit and
  push and with the race detector on, against the 250 ms target
  (`TestEventsLatencyAndPayloads`).
