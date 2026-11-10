# The engine, CLI and MCP server (M1)

This is how milestone M1 implements PRD §8–§11 and §14. The on-disk format is the
[Store Spec](../spec/v1/README.md); this document covers the behavior around it.

## Packages

| Package | Role |
|---|---|
| `pkg/kaspec` | The spec library from M0: parse, canonical format, config layering, validation. |
| `internal/gitx` | Runs the system `git` (2.40+), non-interactively. |
| `internal/engine` | Workspace discovery, the in-memory index, the query language, operations, sync and replay, the merge driver, `ka fix`, and the service API shared by every interface. |
| `internal/render` | Compact text renderings used by both the CLI and MCP. |
| `internal/mcpserver` | `ka mcp`: the MCP tools, resources and prompts over stdio. |
| `internal/api` | `ka daemon`: REST and OpenAPI over HTTP (M2; see [api.md](api.md)). |
| `internal/cli` | The `ka` commands. |
| `internal/testkit`, `internal/scenario` | Throwaway remotes and clones; the PRD scenarios, the multi-clone simulator and the token budgets. |

## Workspaces

`ka` finds its workspace in this order:

1. `--workspace FILE` or `$KA_WORKSPACE`.
2. `.ka-workspace.yml` in the current directory or above.
3. Inside a store (`.ka/store.yml` above the current directory): that store, attached. If the user
   workspace (`~/.config/ka/workspace.yml`; `%AppData%\ka` on Windows) lists the same store, the
   user workspace is used instead, so the other stores come along.
4. Inside a code repo with a `.ka.yml` pointer (`store:` URL or path, `project:`): that store, and
   the checkout is registered as the local path for the project's `repo://` links.
5. The user workspace.

```yaml
# ~/.config/ka/workspace.yml
name: bryan
stores:
  - name: org
    url: git@github.com:KanArtist/ka-org.git   # managed clone
  - name: jk
    path: ~/src/ka/repos/jk                      # attached checkout
    push: manual                                 # optional: immediate | manual | debounce:2s
    fetch_interval: 30s                          # optional: how often the daemon fetches
identity:
  actor: bsant
repos:
  jk: ~/src/oss/jk
```

A store with `url:` is cloned once into the data directory (`$XDG_DATA_HOME/ka/stores`,
`~/Library/Application Support/ka/stores`, `%LOCALAPPDATA%\ka\stores`). A store with `path:` is
an attached checkout. Stores named in `extends` are added automatically, read-only, so config
layering works without listing them. `ka store add|rm|ls` edits the user workspace.

**Identity.** The principal is `identity.actor`, else the actor whose `emails` include git's
author email (`git var GIT_AUTHOR_IDENT`), else a handle derived from that email. An agent acts
as `agent/<name>`: in MCP, `<name>` is the client's `clientInfo.name` ("Claude Code" becomes
`agent/claude-code`); in the CLI, `--as agent/claude` or `$KA_ACTOR`. Agents record the
principal as `on_behalf_of`. `$KA_SESSION` sets the CLI session ID.

## Config layering

A project's data config (workflow, kinds, labels, fields, claims, template) is merged along the
store's own `extends` chain, never the workspace: built-in defaults, then each parent (depth
first, left to right), then the store's `.ka/config.yml`, then the project's `project.yml`. Later
layers win, except for keys a parent `locked:`. Maps merge key by key, `null` deletes, lists are
replaced, and `{$append: [...]}` / `{$remove: [...]}` patch string lists (PRD §9.3).

`ka config explain [--project KEY] [KEY]` prints each effective value and the file and line that
set it, plus locks and runtime settings (`sync.push`, `sync.fetch_interval`) with the workspace
or store recommendation they came from:

```
$ ka config explain --project jk
project jk (store jk)
layers: built-in defaults → org/.ka/config.yml → jk/.ka/config.yml → jk/projects/jk/project.yml

fields.estimate     {type: string, description: ...}   org/.ka/config.yml:17
kinds               [feature, bug, chore, spike]       jk/.ka/config.yml:1   (patched with $append/$remove)
fields.team                                            <deleted by> jk/.ka/config.yml:3
workflow.statuses   [triage (triage), backlog ...]     org/.ka/config.yml:30 (locked by org/.ka/config.yml)
sync.push           debounce:2s                        ~/.config/ka/workspace.yml
```

The same report is `ka_schema {explain: true}` over MCP and `GET /api/v1/projects/{key}/config`.
A parent named by URL is cloned (read-only) even when it is not in the workspace; a parent named
by a relative path (`../org`) is read from that checkout.

## Moving a project between stores

`ka project move KEY --to STORE [--dry-run]` re-homes a project (to split or combine stores,
PRD §9.2). Both stores must be writable members of the workspace. It syncs both, then:

1. copies `projects/KEY/` into the target and commits (`Ka-Moved-From:` trailer);
2. replaces the project in the source with a tombstone `project.yml` (`moved_to_store:` the
   target's URL) and commits (`Ka-Moved-To:`);
3. pushes the target, then the source. If the target push fails the source is not pushed, so
   nobody sees the tombstone without the copy.

IDs and paths inside the project do not change, so `blocked_by`, `parent` and links from other
stores keep resolving. Readers skip tombstones (they never count as a second home); a tombstone
that still has tickets is reported as `moved_project_has_tickets`. Writing to a project whose
store is missing from your workspace says where it went. A clone that wrote to the project
before seeing the move has those operations rejected on replay and preserved as conflicts.
Also `POST /api/v1/projects/{key}/move`.

## Renaming a project

`ka project rename KEY [--key NEW] [--prefix P] [--keep-former-prefix] [--dry-run]` gives a
project a new key, a new ID prefix, or both. Every ticket keeps its suffix (`KA-7QK3M` becomes
`KAN-7QK3M`); its directory and `id` change, and every reference in every writable store follows:
`parent`, `blocked_by`, `related` and the rest, mentions in descriptions, comments and docs, and
`project:<key>` in saved boards and `default_project`. Each store gets one commit; the home store
is pushed first. With `--keep-former-prefix` the old prefix is kept in `former_prefixes`, so old IDs
in commit messages and notes keep resolving (reads and writes) during a transition; drop it from
`project.yml` when no one needs it. Code repos' `.ka.yml` files that name the old key are not
rewritten; the command reminds you. Also `POST /api/v1/projects/{key}/rename`.

`ka store set-url NAME URL [--name NEW]` follows a store's repository to a new URL (a rename or a
transfer): it moves the managed clone, unpushed commits and all, and updates its remote.

## Slugs and references

Every ticket is shown as its reference, the ID plus a slug (`JK-7QK3M-compiler-warnings`, spec
§4.3): in CLI and MCP text, in the `ref` field of JSON results, in commit subjects and in the web
UI's ticket header (click to copy). A ticket without a stored slug shows one derived from its title
(up to three significant words within 24 characters). Every place that takes an ID also takes a
reference, in any case; lookups ignore the slug, so a reference with an old slug still resolves.
Relations are always stored as bare IDs.

Storing slugs is off until a project sets `slugs: true` in its config (or `.ka/config.yml` for the
whole store). Then `ka_create`/`ka new --slug` stores the slug given, normalized ("Compiler
warnings" becomes `compiler-warnings`) and checked against the limits (4 words, 24 characters;
too long is an error, never cut short), or one derived from the title. `ka edit --slug` changes it;
renaming the title does not. While slugs are off, a given slug is checked but not stored, and
setting one with `ka edit` is refused.

ka before 1.2.0 rejects a ticket with a slug, so turn slugs on for a store in this order:

1. Upgrade every client of the store (every machine, CI job and action that runs `ka`) to 1.2.0 or
   newer.
2. Add `requires_ka: 1.2.0` to the store's `.ka/store.yml`, commit and push. Any client still older
   than that now stops with an explanation instead of failing on individual tickets.
3. Set `slugs: true` in the config, commit and push.
4. Run `ka fix --slugs` once to give existing tickets a slug derived from their titles (one commit
   per store). It refuses while step 2 is missing.

`ka check` warns (`slugs_unguarded`) when a store has slugs but no such `requires_ka`.

## Version requirements

A store's `requires_ka` (spec §5.1) names the oldest ka release that may use it. A ka older than
that refuses the store: `ka` commands and `ka mcp` fail to start with `ka_outdated` and how to
upgrade; writes are refused; and sync will not bring in a remote whose `store.yml` raised the
requirement, so an older ka never reads or rewrites data it does not understand. A write made after
the remote raised it stays committed locally and reports why it was not pushed. Development builds
(versions with a `-` suffix) are never refused. `ka check` and `ka doctor` report it as
`ka_outdated`.

## The index

All tickets, comments, projects, configs and actors are loaded into memory, in parallel. The
migrated `jk` store (3,648 tickets) loads in about 0.1 s, so the CLI simply builds it on every
invocation. Long-running processes (`ka mcp`) update it incrementally. After each write the
changed paths are reloaded. Before each call, a stat of `.git/logs/HEAD` detects commits made by
other processes sharing the clone; the diff since the last known HEAD is then reloaded. A change
to config, a project file or a board rebuilds everything.

## Operations

Every write is one typed operation: `create`, `update`, `claim`, `release`, `transition`,
`comment` or `check`. Applying one:

1. takes the clone's write lock (`gofrs/flock`: `flock` on Unix, `LockFileEx` on Windows, on
   `.git/ka.lock`);
2. reads the ticket from disk and checks preconditions: it exists and was not moved; a claim
   requires the ticket to be unclaimed, claimed by the same actor, or stale with `steal`;
3. validates the result against the project's vocabulary (statuses, kinds, closed labels,
   declared fields, existing references). Errors carry a code and a fix, and suggest the nearest
   valid value (`unknown_status: "doing" is not a status (fix: did you mean in_progress?)`);
4. writes canonical bytes, commits once with a feed-style subject and trailers (`Ka-Op`,
   `Ka-Actor`, `Ka-On-Behalf-Of`, `Ka-Session`, `Ka-Spec`), and pushes.

Batches (`ka_create`, `ka_update`) make one commit per ticket and push once. Claims always push
synchronously, even when the store is set to `push: manual`. A claim that cannot be pushed
(offline) is reported as `confirmed: false`. Closing a ticket (a `completed` or `canceled`
status) requires a summary, which is committed as a `resolution` comment in the same commit, and
reports the tickets it unblocked. Progress comments are throttled per agent and ticket
(`comments.progress_min_interval`; `progress_throttled` says when to retry).

## Sync and replay

`ka` keeps history linear and never creates merge commits. When a push is rejected, `ka`
fetches. If the clone is behind, it fast-forwards. If it has diverged, `ka` replays rather than
rebases:

- **Commits `ka` made** (they carry `Ka-Op`) are re-applied *as operations* on top of upstream.
  Their preconditions are checked again, so a claim that someone else won fails with `claim_lost`
  and leaves no commit. `ka` never text-merges its own commits. Replay keeps each commit's
  original author and date. If a `create` collides with an ID published meanwhile, the ticket gets
  a new random ID, later operations are remapped to it, and the rename is reported (`renamed`).
- **Other commits** (hand edits) are cherry-picked. `ticket.md` goes through the semantic merge
  driver. Any other conflicting file keeps the upstream version, and the local version is
  preserved.
- An operation that can no longer be applied (other than the caller's own, which returns its
  error) is preserved as a `system` comment on its ticket, with the operation's JSON.

If the clone and upstream share no commit at all, `ka` stops with `git_error` and touches
nothing. That happens when the remote URL now names a different repository, for example after
the store's repository was renamed and its old name reused; replaying would publish the store
there. Fix the remote (`ka store set-url`, or `git remote set-url` in an attached checkout).

The push is retried up to 8 times, with jittered exponential backoff. If every attempt fails, the
commits stay local and the result says why.

**Push modes and fetching** (PRD §10.1). A store pushes `immediate`ly by default. `manual` commits
locally only. `debounce:2s` batches bursts: each write commits at once, and the push follows once
the store has been quiet for the delay, or at most five delays after the first pending commit.
Results say `push pending` until then. Only long-running processes (the daemon, `ka mcp`)
debounce, and they flush pending pushes before exiting; a CLI command pushes immediately. Claims
always push synchronously, and they carry any pending commits with them. The daemon fetches each
store every `fetch_interval` (15 s by default), and immediately on `POST /api/v1/hooks/git`.

Both settings come from the workspace's `stores:` entry when it sets them (the user wins). Otherwise
they come from the store's recommendation, `runtime.sync.push` and `runtime.sync.fetch_interval` in
`.ka/config.yml`, which follows `extends`.

**The merge driver** (`ka merge-driver %O %A %B %P`, registered by `ka init` and
`ka doctor --fix`; `.gitattributes` routes `ticket.md merge=ka`) merges frontmatter field by
field:

- set fields: 3-way set merge;
- acceptance items: matched by text, and `done` from either side wins;
- `updated`: the later value;
- the body: `git merge-file`;
- a scalar changed on both sides: the published side wins.

Losing values are written to `.git/ka-preserved/`, because a merge driver cannot commit. The next
`ka` write or sync turns them into a `system` comment. Without `ka` installed, git falls back to
its text merge.

## Queries

The query language is shared by the CLI, MCP and (in M2) REST. Terms are ANDed; values separated
by commas are ORed; a leading `-` negates a term; `*` is a wildcard; `none` matches an empty
field; bare words search the title and description.

```
project:jk,gyggly category:started -label:infra assignee:agent/* priority:P0,P1
is:ready | is:open | is:closed | is:blocked | is:claimed | is:stale | is:question
has:parent  blocked_by:JK-7QK3M  blocks:JK-7QK3M  field.estimate:M  sort:-updated
```

`ka_next` considers tickets that are in an `unstarted` status, unclaimed, not blocked (a
`blocked: true` status, or an open ticket in `blocked_by`; references outside the workspace do
not block) and assigned to nobody or to the caller. Tickets with open children are excluded: the
work is in the children. Candidates are ordered by priority, then age, with the caller's own
assignments first. The chosen ticket moves to the first `started` status as it is claimed.

## MCP

Configure it in Claude Code with `claude mcp add ka -- ka mcp`. Run it from inside a store or a
code repo with `.ka.yml`, or set a user workspace. When a `ka daemon` runs for the same workspace,
`ka mcp` proxies to it ([api.md](api.md#mcp-over-http-mcp)).

Tools:

- `ka_next`, `ka_search`, `ka_get`, `ka_create`, `ka_update`;
- `ka_claim`, `ka_release`, `ka_transition`, `ka_comment`, `ka_check`;
- `ka_ask` (with `wait`), `ka_inbox`, `ka_schema`, `ka_sync`, `ka_resolve_link`.

Input schemas list the workspace's real projects, statuses, kinds, closed labels and custom
fields as enums. Results are compact text plus `structuredContent`; errors are tool results with
`isError`, a code, and a fix. Ticket text from others is labeled as data. Resources are
`ka://ticket/{id}` and `ka://board/{name}`; prompts are `ka/plan-epic` and `ka/write-ticket`.

**Token budgets** (PRD §11.3) are regression tests (`internal/scenario/budget_test.go`). They
count the arguments the model writes plus the text it reads, using a deliberately conservative
estimator: one token per 4 letters, per 3 digits, and per punctuation character. On realistic
fixtures, `ka_next` costs about 510 tokens, `ka_comment` 65, `ka_transition` 60 and a 20-result
`ka_search` about 1,070. `ka_next` fits its digest to 740 tokens: the description gets whatever
room the rest leaves. Over every ready ticket of the migrated `jk` store, the maximum is 760
(`KA_BUDGET_STORE=<store> go test ./internal/scenario -run RealStore`).

## Tests

- `internal/scenario` runs the M1 exit scenarios against real git remotes and clones:
  - S1: two agents call `ka_next` at once over MCP and get different tickets, each claimed and
    pushed;
  - S4: five follow-ups created in one call, pushed once;
  - S6: a hand-written CRLF ticket is picked up, reported and fixed;
  - S8: closing unblocks dependents;
  - S9: `ka approve <ID>` works exactly as an agent would write it.
- The M2 scenarios run through a daemon, with the agent on the `ka mcp` bridge and the human on
  the UI's REST and SSE endpoints:
  - S2: progress notes and acceptance checks reach a watching client in about 15 ms;
  - S3: a `ka_ask` with options appears in the human's inbox; the human answers as the UI does;
    and the agent's waiting `ka_ask` returns the chosen option.
- **S7** builds an org store (shared config with a lock, actors, an `in-flight` board, a project
  of its own) and two project stores extending it. It checks inherited and patched kinds, the
  lock, `ka config explain`, a jk ticket blocked by a gyggly ticket (unblocked when that closes),
  and one board spanning all three stores. `KA_S7_STORES=<migrate-v0 --out dir>` runs the same
  checks over real migration output (split or single layout).
- **S5** is a simulator: 20 agents in 12 processes on 6 clones perform about 270 random
  operations (next, claim, progress, check, close, create, label, comment, sync). It then checks
  four things: every accepted change is present in a fresh clone; IDs are unique; history is
  linear; and every process converged.

## Not yet built

- `ka migrate` (a store upgrade path) waits for a Store Spec v2.
- The roadmap/milestones view, manual ranking, importers and notifications are later work
  (PRD §21).
