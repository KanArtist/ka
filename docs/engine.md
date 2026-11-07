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
    url: git@github.com:JumpKickOSS/ka-org.git   # managed clone
  - name: jk
    path: ~/src/ka/repos/jk                      # attached checkout
    push: manual                                 # optional: commit locally only
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

The push is retried up to 8 times, with jittered exponential backoff. If every attempt fails, the
commits stay local and the result says why.

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
code repo with `.ka.yml`, or set a user workspace.

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
- **S5** is a simulator: 20 agents in 12 processes on 6 clones perform about 270 random
  operations (next, claim, progress, check, close, create, label, comment, sync). It then checks
  four things: every accepted change is present in a fresh clone; IDs are unique; history is
  linear; and every process converged.

## Not in M1

- The daemon, REST, SSE and the web UI arrive in M2. So does proxying `ka mcp` to a daemon.
- `runtime.sync.push: debounce` is not implemented. Stores push immediately unless the workspace
  sets `push: manual`.
- `ka watch`, `ka open`, `ka config explain`, `ka project move`, `ka migrate`, `ka hooks install`,
  `ka completion`, `ka check --ci` annotations and SARIF, and interactive prompts for missing
  arguments are not implemented.
