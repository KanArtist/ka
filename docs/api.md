# The daemon and REST API (M2)

`ka daemon` serves one workspace over HTTP: REST under `/api/v1`, described by an OpenAPI 3.1
document. It is the same `engine.Service` that `ka mcp` and the CLI use, so behavior cannot
diverge (PRD §12.1). SSE, MCP over HTTP, presence and the web UI build on it (see the M2 tickets).

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

## Security (PRD §15, local mode)

- The daemon listens on loopback only. Non-loopback addresses are refused until remote mode (M4).
- Every request except `GET /healthz` and `GET /auth` needs the token: `Authorization: Bearer
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
| `GET /daemon`, `POST /daemon/shutdown` | | `ka daemon status`, `stop` |
| `GET /openapi.json` | | `ka daemon openapi` |

A test (`TestEveryServiceMethodHasAnEndpoint`) fails when a new exported `engine.Service` method
has no endpoint. Another validates `openapi.json` against the official OpenAPI 3.1 schema.

## Measured

On the migrated `jk` store (3,648 tickets), warm `GET /tickets?q=…` requests take about 1 ms end
to end over loopback, against the PRD's 20 ms target.
