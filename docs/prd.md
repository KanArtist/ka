# KanArtist (`ka`): PRD

**Status:** draft v1 (all open decisions resolved 2026-11-07). M0 and M1 delivered: the Store Spec in [`spec/v1/`](../spec/v1/README.md) is the normative reference for §7 and §9.3 (where they differ, the spec wins); the engine, CLI and MCP stdio server are described in [`docs/engine.md`](engine.md).
**Owner:** Bryan Sant
**Created:** 2026-11-07
**Supersedes:** `kanartist/docs/kanartist-prd.md` (the Python v0)

> **KanArtist** is a work tracker built for agents, with git as its database. Humans get a live
> window into what the agents are doing. (Kanban Artist, and also a nod to the con artist.)

---

## 1. Summary

KanArtist replaces Jira, Linear, and GitHub Projects for teams whose work is mostly done by AI
agents. Those tools were built for humans, and their APIs and MCP servers were added later. KanArtist
is the other way around:

- **Agents are the primary users.** The MCP and CLI surfaces are the main product. They are designed
  to use few tokens, to be hard to misuse, and to work the same way every time.
- **Humans are mostly watching.** A live web UI (in the style of Linear and GitHub Projects, kept
  current over SSE) shows what is happening right now. People can step in when they want to, or
  when an agent asks them to.
- **Git is the source of truth.** All state lives in one or more git repos that follow a published,
  versioned **Store Spec**. Indexes, caches, and presence data are disposable and can always be
  rebuilt from git.
- **It is decentralized.** If you can clone the repos and push to them, you can run KanArtist
  locally, use a remote instance, or use nothing at all (hand-written files that follow the spec are
  valid).
- **It ships as one Go binary** (`ka`) with the Angular UI embedded in it. It runs on Linux, macOS,
  and Windows. The same binary is the CLI, the daemon (`ka daemon`: web server, REST API, and MCP
  over HTTP), and a stdio MCP server (`ka mcp`).

---

## 2. Background: what v0 taught us

v0 was a sidecar repo of markdown tickets with a Python CLI. It ran in heavy production use from
2026-07-20 to 2026-11-07: **3,959 tickets, 8,455 commits, up to 219 commits a day**, with many
concurrent agent sessions. The idea held up. The details did not. Every lesson below comes from the
v0 corpus and its history.

| # | Evidence in v0 | Lesson for v1 |
|---|---|---|
| L1 | Two different tickets are both `JK-4663` right now. Three commits renumber colliding IDs ("renumber my JK-4644/4645 (collided with another session's)"), and one removes "duplicates of JK-4235 and JK-4237 from a retried push". | **Sequential IDs fail under concurrent agents**, even when the push settles the allocation. The colliding files have different names, so git merges them cleanly and the collision goes unnoticed. Agents writing tickets by hand skip allocation entirely. **IDs must need no coordination.** |
| L2 | 112 tickets have CRLF line endings (for example, `status: done\r`). | Line endings and formatting must be canonical and enforced (`.gitattributes`, `ka fix`). A Windows edit must not quietly fork the data. |
| L3 | The same person appears as `bsant`, `bryan.sant`, `"Bryan Sant"`, and `Bryan Sant`, plus 24 tickets assigned to `unknown`. Agents show up as `claude`. | We need an **actor registry** with aliases, and a clear model of *agent working on behalf of a human*. |
| L4 | `kind: build\|test\|lock\|update\|format\|native\|image\|assemble\|compile\|clean` landed on `main` (JK-2008). | Validating "in CI after the push" is too late. Enforce at the point of writing (MCP and CLI schemas), and make hand-written mistakes easy to fix automatically. |
| L5 | Tickets created from the template keep their placeholder sections ("_What's wrong…_", `- [ ]`), and the real content is appended below them (JK-4661). | Creating a ticket should take **structured input** and render the body. Never write placeholders. |
| L6 | Ticket bodies collect sections added after the fact: `## Outcome` (391), `## Resolution` (295), `## Done` (143), `## Closing note` (102), `## Shipped` (74), `## Closed by audit (2026-09-03)` (61), and more. | Agents need **comments and resolutions as first-class, append-only records**. Editing a shared body to add history is how merge conflicts start. |
| L7 | Commit messages from the tool (`JK-4618 → done (claude)`) are mixed with hand-written batch commits (`jk: JK-4661 … done; follow-up JK-4663`). | Build the activity feed from **diffs plus structured commit trailers**, not by parsing commit subjects. Tolerate batch commits. |
| L8 | `claimed_at` holds only a date. 22 tickets are `in_progress` with no way to tell which are stale. | Claims need full timestamps, a session identity, and an explicit staleness policy. |
| L9 | 3,649 files sit in one directory. 93% of the corpus is `done`. Archiving moved files (`archive/2026/`) for only one project. | Paths must be stable forever, so links never break and nothing is renamed. Query speed is the index's job, not the directory layout's. |
| L10 | `EXTENSION_KEYS` is a hard-coded registry (`branch`: 279 tickets, `source`: 275). | Teams need **declared custom fields** in config, validated like built-in fields. |
| L11 | Python runtimes, venvs, pip, and uv behave differently on each OS, so the tool "doesn't work all the time". Agents fall back to hand edits, which are slower, inconsistent, and expensive in tokens. | **One static binary with no runtime.** Installing must take less than a minute everywhere. |
| L12 | Projects carry substantial docs: `projects/jk/docs/{prds,audits,perf}` holds roughly 100 files. | Project docs (PRDs, audits, briefs) are a real need and should be first-class content, not an afterthought. |
| L13 | An unknown-key lint (`statu: ready`) and a closed vocabulary caught real mistakes. | **Keep these.** A strict schema is a feature. |

---

## 3. Goals and non-goals

### 3.1 Goals

1. **Agent-first ergonomics.** Every common agent action (find work, claim, update, comment, ask a
   human, close) takes one tool call, with a compact response and no guessing about vocabulary.
2. **Git-native source of truth.** Everything durable lives in git repos that follow the Store Spec.
   Any number of KanArtist instances, or none, can work on the same repos.
3. **Observability for humans.** A live, good-looking UI shows what agents are doing *right now*:
   claims, progress, questions, and transitions. It answers "what is happening?" within seconds.
4. **Humans can intervene when needed.** Comment, answer an agent's question, approve, re-prioritize,
   reassign, create, or move a ticket. These are first-class actions, but they are not the main use
   case.
5. **Conflicts are handled automatically.** Concurrent edits from many agents across many clones are
   resolved without human help in more than 99% of cases. No data is ever silently lost.
6. **Layering across repos.** A workspace combines many stores (one per project, per team, or per
   board, or all projects in one store), and config layers through explicit `extends` chains.
7. **Portability.** Linux, macOS, and Windows, from one codebase and one release pipeline. A local
   install works out of the box. A remote install is optional.
8. **Hand-written files are a supported path.** The spec is simple enough that an agent with only
   `git` and a text editor can produce valid data. `ka fix` normalizes anything close to valid.
9. **An open-source product.** A clear spec, conformance tests, docs, packaging, and an
   extensible design that other teams will adopt.

### 3.2 Non-goals (v1)

- **Not human-first.** We will not add features whose main value is human workflow ceremony, such as
  sprint planning poker, burndown theater, or time tracking.
- **No authoritative database.** Nothing outside git is ever the source of truth. Side stores are
  caches.
- **No non-git backends.**
- **No real-time co-editing of ticket bodies** (no CRDTs). Rare concurrent body edits are handled by
  the conflict policy (§10).
- **No fine-grained RBAC in v1.** Write access comes from git push access and daemon authentication.
- **Not a code review tool.** PRs stay on the forge. KanArtist links to them.
- **No hosted SaaS** run by the project. Self-host only.
- **No two-way sync with Jira or GitHub Issues in v1.** One-way importers may come later (§21).
- **No backward compatibility with v0.** Migration is a one-time, manual operation (§20), and the v0
  layout is explicitly not a constraint.

---

## 4. Users and core scenarios

### 4.1 Personas, in priority order

1. **Coding agent (primary).** Claude Code, Codex, Cursor, Gemini CLI, custom Agent SDK agents, and
   CI bots. They connect through MCP (stdio or HTTP) or the CLI with `--json`. They care about token
   cost, determinism, clear errors, and not stepping on other agents.
2. **Agent orchestrator.** A lead agent that splits work into tickets, fans them out to sub-agents,
   and reviews the results. It needs bulk creation, parent/child relations, dependency graphs, and
   claim visibility.
3. **Watching human.** A developer or lead who keeps the board open on a second monitor. They care
   about the live activity feed, which agents are working on what, what is blocked, and what needs
   their input.
4. **Intervening human.** The same person, acting. They answer a question, approve a design, fix a
   priority, or file a ticket.
5. **Operator.** Runs a shared remote daemon for a team. They care about auth, webhooks, uptime, and
   resource use.

### 4.2 Core scenarios (acceptance tests for the product)

- **S1: Find and claim.** An agent in a `jk` checkout calls `ka_next`. It gets exactly one
  unblocked, highest-priority ticket with the context it needs, already claimed for its session. The
  claim is pushed before the call returns. A second agent calling at the same moment gets a
  different ticket.
- **S2: Progress, visible live.** The agent posts progress notes and checks off acceptance items. A
  human watching the board sees each change appear within about a second (from the local daemon) or
  within one fetch interval (from a remote one).
- **S3: Ask a human.** The agent hits a design fork and calls `ka_ask` with a question and options.
  The ticket appears in the human's **Inbox** with a badge. The human picks an option in the UI (or
  runs `ka answer`). The agent, waiting on `ka_ask(wait=true)` or polling, receives the answer and
  continues.
- **S4: File follow-ups.** While working, an agent finds five related defects. It creates them in one
  `ka_create` call (a batch) with structured fields. Each gets a collision-free ID with no network
  round-trip, and each links back to the parent ticket.
- **S5: Concurrent chaos.** Twenty agents on twelve machines update tickets in the same store for a
  day. No duplicate IDs. No lost updates. No human needed to resolve conflicts. Any edit that could
  not be applied is surfaced as a visible event with the content preserved, never silently dropped.
- **S6: Hand-written ticket.** An agent without `ka` writes a ticket file by hand following
  `AGENTS.md` and pushes it. The daemon picks it up. `ka check` reports any defects and `ka fix`
  corrects formatting.
- **S7: Layered workspace.** A developer's workspace combines `org-planning` (shared config, actors,
  and cross-team boards), `jk-tickets`, and `gyggly-tickets`. One board shows `in_progress` work
  across all three, and a `jk` ticket can be blocked by a `gyggly` ticket.
- **S8: Close the loop.** An agent finishes the work and calls `ka_transition(done, resolution=…)`.
  The resolution is recorded as a resolution comment, the ticket moves columns on every watcher's
  board, and dependents become unblocked and show up in `ka_next`.
- **S9: Point a human at an action.** An agent unsure about a destructive step tells the user: "run
  `ka approve JK-7QK3M`". The CLI command is exact, so no explanation of where to click in the UI is
  needed.

---

## 5. Product principles

1. **Git is the database. The tool is an accelerator, not a gatekeeper.** Litmus test for every
   feature: can its result be represented as files in the store? If not, either it is ephemeral
   (presence, live streams, caches) and labeled as such, or it does not ship.
2. **Store every fact once, in the file that owns it.** Inverse relations (blocks/blocked-by,
   children/parent) are derived. A single logical change should touch a single file.
3. **Append rather than edit.** Comments, questions, answers, and resolutions are new files. Shared
   files are edited only for current-state fields.
4. **Canonical bytes.** Serialization is deterministic (key order, quoting, LF line endings, trailing
   newline), so diffs stay minimal and merges stay clean.
5. **Strict input, forgiving repair.** Writes through `ka` are validated strictly. Data that is close
   to valid (hand-written or legacy) is repaired by `ka fix`, never silently guessed at read time.
6. **Have strong opinions about git.** Use linear history, small commits (one operation per commit),
   push early, and rebase always. Conflicts are resolved by replaying *intent*, not text.
7. **Spend as few agent tokens as possible.** Use compact responses, field selection, coarse-grained
   tools, dynamic enums in tool schemas, and errors that say how to fix the problem.
8. **Humans watch first.** The UI's default experience is a live, legible view of activity. Editing
   is available but never required.
9. **Same behavior on every OS.** No feature depends on a platform-specific IPC mechanism. Where an
   OS difference cannot be avoided (file locking, paths), it is isolated behind an interface.

---

## 6. Concepts and glossary

| Term | Definition |
|---|---|
| **Store** | A git repo, plus an optional branch and subdirectory, that holds KanArtist data in Store Spec layout. It is identified by `.ka/store.yml`. |
| **Workspace** | The set of stores a `ka` instance works on, in layering order (§9). It is defined by local config or discovered from pointer files. |
| **Project** | A container of tickets with a unique **key** (`jk`) and **ID prefix** (`JK`). It lives in exactly one store. It is the equivalent of a Linear team or a GitHub repo's issues. |
| **Ticket** | A unit of work. A directory holding `ticket.md` (current state) plus append-only comments and attachments. |
| **Comment** | An append-only record on a ticket. Its type is one of `note`, `progress`, `question`, `answer`, `decision`, `resolution`, or `system`. |
| **Board** | A saved, cross-project view (filters, grouping, columns, sort) in the style of GitHub Projects. It can live in any store. |
| **Actor** | A human (`bsant`) or an agent (`agent/claude`). Defined in the actor registry with aliases and git emails. |
| **Session** | One agent's connection (one MCP session or one CLI invocation chain). Ephemeral, but its ID is recorded on claims and in commit trailers. |
| **Claim** | A ticket's lock-like assignment to an actor and session, published by a push. Can go stale. |
| **Workflow** | A project's named statuses, each mapped to a fixed **category**. |
| **Spec** | The versioned contract for store layout and file formats (`spec/v1`). It is the real interface. `ka` is one implementation. |

---

## 7. The Store Spec (git schema)

The spec is the product's most important artifact. It is versioned (`spec: 1` in
`.ka/store.yml`), documented in `spec/v1/README.md`, backed by JSON Schemas, and tested by a
conformance corpus. Any tool that reads and writes this layout correctly is a valid KanArtist client.

### 7.1 Alternatives considered

#### Where the data lives

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **A. Dedicated sidecar repo** (v0) | No churn in code repos. Easy to browse on the forge. Normal clone, push, and PR workflow. Supports cross-project stores. | One more repo to know about (fixed by pointer files, §9.4). | **Default** |
| **B. Orphan branch in the code repo** (for example, `ka`) | One repo. Ticket commits never touch `main`'s history. | Forge UIs hide it. CI triggers need care. Some agents get confused by multiple branches. | **Supported** for free: a store is `url + branch + subdir`. |
| **C. Custom refs/objects** (`refs/ka/*`, git-bug style) | Invisible, no working tree needed. | Not browsable. Needs special refspecs to clone and push. Cannot be written by hand. Breaks "anyone with git can participate". | Rejected |
| **D. Subdirectory of the code repo's `main`** (pre-v0 JumpKick) | Simplest possible setup. | This is exactly the churn we left. | Rejected (allowed only by pointing a store at a subdir, at your own risk) |

#### How ticket data is split into files

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **A. One mutable markdown file per ticket** (v0) | Simplest. Easy to grep. | History accumulates in the body (L6), so concurrent comments conflict. | Rejected as the full model |
| **B. Event-sourced: one file per event, state derived by folding events** | Conflict-free (only additions). Perfect audit trail. | State is unreadable without the tool. Hard to write by hand. Grep does not work for "status: ready". | Rejected |
| **C. Hybrid: current-state `ticket.md`, append-only `comments/*.md`, and git log as the event stream** | Grep works. Comments never conflict. State edits are small edits to frontmatter fields. Git history already is an event log. | Two concepts to learn (state versus comments). | **Chosen** |

#### Ticket IDs

| Option | Example | Coordination | Readability | Verdict |
|---|---|---|---|---|
| Sequential per project (v0) | `JK-4661` | Needs a network round-trip. Races produce silent duplicates (L1). Agents writing by hand cannot do it safely. | Best | Rejected |
| ULID or UUID | `01JC3Z…` (26 chars) | None | Poor. Costs more tokens. | Rejected |
| Date plus slug | `JK-20261107-cache-fix` | Low (slug collisions) | Medium. Long. Unstable if titles change. | Rejected |
| **Prefix plus short random Crockford base32** | `JK-7QK3M` | **None.** Unique locally by construction. The rare remote collision is detected and fixed automatically while the ticket is unpublished. | Good. Short, case-insensitive, no ambiguous characters (I, L, O, U). | **Chosen** (decision D1) |

ID rules:

- Format: `^[A-Z][A-Z0-9]{0,9}-[0-9A-HJKMNP-TV-Z]{5,8}$`. The default suffix length is 5, giving
  about 33.5 million values per project. A store can raise it in config.
- Collision analysis: `ka` checks every ID it can see locally before choosing one, so a collision
  needs two tickets created *in parallel, without seeing each other, before a sync*. Even with 100
  such tickets in one window, p ≈ 1.5×10⁻⁴. If one happens, `ka sync` detects it on rebase. The
  ticket that has *not yet been pushed* gets a new ID, and references to it in other unpushed changes
  are rewritten. Published IDs never change.
- An agent writing by hand generates 5 random characters from the alphabet. `ka check` flags
  low-entropy suffixes (such as `AAAAA`) and duplicates.
- IDs are immutable. Moving a ticket to another project creates a new ID and leaves a tombstone
  (`moved_to:`) at the old path.

#### File format

| Option | Verdict |
|---|---|
| **Markdown with YAML frontmatter** | **Chosen.** It is the de facto convention, GitHub renders frontmatter as a table, and every agent knows it. We use a **restricted YAML profile**: no anchors or tags, no implicit typing (avoids the "Norway problem"; dates and times are strings validated by pattern), block-style lists one item per line, and keys in canonical order. |
| TOML frontmatter (`+++`) | Fewer YAML pitfalls, but less familiar to agents and not rendered by forges. |
| Separate JSON metadata file plus Markdown body | Two files per change. Agents are more likely to desynchronize them. |

### 7.2 Layout

```
<store root>/                       # repo root, or a subdirectory of it
├── .ka/
│   ├── store.yml                   # spec version, store id, name, `extends` parents (required: marks a store)
│   ├── config.yml                  # workflows, kinds, labels, custom fields, policies (layers via `extends`, §9.3)
│   └── actors.yml                  # actor registry (optional; usually in the org store)
├── .gitattributes                  # `* text=auto eol=lf` and `ticket.md merge=ka` (written by `ka init`)
├── AGENTS.md                       # generated hand-writing protocol (`ka agents-md`), optional
├── projects/
│   └── jk/
│       ├── project.yml             # key, prefix, name, code repos, project-level config overrides
│       ├── tickets/
│       │   └── JK-7QK3M/
│       │       ├── ticket.md       # current state: frontmatter + description
│       │       ├── comments/
│       │       │   ├── 20261107T130722Z-4F2A.md
│       │       │   └── 20261107T141005Z-9KQ1.md
│       │       └── files/          # attachments (logs, screenshots); size-capped, LFS-friendly
│       └── docs/                   # project docs (PRDs, audits, briefs); plain markdown
├── boards/
│   └── agents-in-flight.yml        # saved cross-project views
└── milestones/                     # (P1) milestones / cycles, one file each
    └── jk-1.0.yml
```

Rules:

- **Paths are a pure function of the ID**: `projects/<key>/tickets/<ID>/ticket.md`. They never move.
  There is no archive directory (L9).
- File and directory names must be safe on Windows and on case-insensitive filesystems: no `:`, no
  case-only differences, short paths (well under 260 characters total), and ASCII only.
- Comment filenames are `<UTC yyyymmddThhmmssZ>-<4 random base32>.md`, so they sort by time and
  never collide. Authorship goes in the frontmatter, not the filename.
- Unknown files under `tickets/<ID>/` are allowed only under `files/`. Anything else is a `ka check`
  warning.

### 7.3 `ticket.md`

```markdown
---
id: JK-7QK3M
title: Incremental compile keeps a deleted class in a merging processor's output
status: in_progress
priority: P1
kind: bug
labels:
  - cache
  - compile
assignee: agent/claude
owner: bsant
claim:
  by: agent/claude
  session: 01JC4N7Q2V8X
  at: 2026-11-07T13:07:22Z
parent: JK-3F9KA
blocked_by:
  - GYG-K2M7P
links:
  - jk://compiler/IncrementalCompile.java@3f2a9c1#L42
  - https://github.com/jkbuild/jk/pull/912
acceptance:
  - text: A deleted @Service class is absent from service-registry.json after incremental compile
    done: true
  - text: The compile cache key covers prior generated resource output
    done: false
fields:
  estimate: M
  branch: jk-7qk3m-processor-merge
created: 2026-11-07T12:58:03Z
created_by: agent/claude
updated: 2026-11-07T13:07:22Z
---

## Problem

Helidon's processor reads the existing `service-registry.json` and merges into it, so …

## Approach

…
```

Field reference. Fields marked **R** are required. All others are optional, and empty values are
omitted rather than written as `""` or `[]`.

| Field | Type | Notes |
|---|---|---|
| `id` **R** | ID | Must match the directory name. |
| `title` **R** | string | One line, at most 300 characters (v0 data: p99 is 245). |
| `status` **R** | status name | Must exist in the project's workflow (§7.6). |
| `priority` | `P0`–`P3` | Missing means "none". Sorted P0 first. |
| `kind` | enum | From config. Default vocabulary: `feature, bug, chore, research, design, refactor, perf`. |
| `labels` | list of strings | From config if the project declares a closed label set. Otherwise free-form. Stored sorted. |
| `assignee` | actor | The actor doing the work now (may be an agent). |
| `owner` | actor (human) | The accountable human. Defaults to the creator's principal. |
| `claim` | `{by, session, at}` | Present only while claimed. `at` is an RFC 3339 UTC timestamp (L8). |
| `parent` | ID | For sub-issues and epics. The child stores this field. Children are derived. |
| `blocked_by` | list of IDs | May cross projects and stores. `blocks` is derived. |
| `related`, `duplicate_of` | ID or list of IDs | Each fact is stored once, on the ticket that declares it. |
| `links` | list of strings | Symbolic code references (`repo://path[@ref][#Lline]`, §7.8) or URLs. |
| `acceptance` | list of `{text, done}` | First-class checklist. The UI shows progress (3/5). Agents check items through a tool (L5). |
| `milestone` | milestone key | P1 |
| `due` | date | P2 |
| `fields` | map | Custom fields **declared in config** with types (string, number, enum, date, actor, url). Undeclared keys fail validation (L10, L13). |
| `moved_to` | ID | Tombstone only. Present only on moved tickets. |
| `created` **R**, `updated` **R** | RFC 3339 UTC | `ka` stamps them. `ka fix` stamps them on hand-written files. |
| `created_by` | actor | |

Body: free Markdown. The project's template defines recommended sections (default: `Problem`,
`Approach`, `Non-goals`, `Notes`). Acceptance criteria live in frontmatter, not in the body. **Unknown
top-level frontmatter keys are errors** (the `statu:` rule from v0).

### 7.4 Comments

```markdown
---
type: question
author: agent/claude
on_behalf_of: bsant
session: 01JC4N7Q2V8X
created: 2026-11-07T14:10:05Z
to: bsant                # optional addressee (actor or role); drives the Inbox
options:                 # optional, for multiple-choice questions
  - Mark the processor non-incremental (Gradle's approach)
  - Snapshot and diff generated resources per compile
---

Two viable fixes; the first is safe but costs full recompiles for Helidon modules. Which?
```

- Types:
  - `note`: general remarks.
  - `progress`: agent work-log entries. The UI can collapse them.
  - `question`: an open request for human input.
  - `answer`: has `answers: <comment filename>` and optionally `choice: <index>`.
  - `decision`: a recorded design decision. It is pinned in the UI.
  - `resolution`: written on close, with `resolution: done|canceled|duplicate|wontfix`.
  - `approval`: answers an approval request, with `approved: true|false`.
  - `system`: written by `ka` for preserved conflict content (§10.4), moves, and similar events.
- **Comments are immutable.** An edit is a new comment with `supersedes:`. A deletion is a new comment
  with `retracts:`. The rare true deletion (for example, a pasted secret) is a normal git commit
  that removes the file, plus a history rewrite if needed. This is documented, but `ka` does not
  automate it.
- A question's open or closed state is **derived**: it is open until an `answer` references it. No
  file is ever edited to close a question.

### 7.5 Config (`.ka/config.yml`; layering rules in §9.3)

```yaml
id_suffix_length: 5
workflow:
  statuses:
    - { name: triage,      category: triage }
    - { name: backlog,     category: backlog }
    - { name: ready,       category: unstarted }
    - { name: in_progress, category: started }
    - { name: blocked,     category: started, blocked: true }
    - { name: review,      category: started }
    - { name: done,        category: completed }
    - { name: canceled,    category: canceled }
  initial: backlog
  agent_created_initial: triage   # optional policy: agent-filed tickets land in triage for a human
kinds: [feature, bug, chore, research, design, refactor, perf]
labels: { closed: false, known: [cache, compile, infra] }
fields:
  estimate: { type: enum, values: [XS, S, M, L, XL] }
  branch:   { type: string }
claims:
  stale_after: 24h        # stale claims show in the UI and can be taken with --steal
  push_required: true     # a claim is not confirmed until it is pushed
comments:
  max_bytes: 65536
  progress_min_interval: 2m   # throttle agent progress notes per ticket (D8); 0 disables
attachments:
  max_bytes: 5242880
locked: [workflow]        # child layers may not override these keys (§9.3)
runtime:                  # Class 3 recommendations; the user's settings win (§9.3)
  sync:
    push: immediate       # immediate | debounce:2s | manual
    fetch_interval: 15s
```

Everything outside `runtime:` is **data config** (Class 1 in §9.3). It is resolved along the store's
`extends` chain and is the same for every client.

**Workflow categories are fixed** (`triage`, `backlog`, `unstarted`, `started`, `completed`,
`canceled`), following Linear's model. Status *names* are configurable per project, but agents and
the tooling reason about categories. That keeps v0's "status means the same thing everywhere"
property (cross-project tools work) while letting teams name their states. `canceled` is new, and
fills a gap v0 had (duplicates and "won't do" were handled by deleting files).

### 7.6 Actors (`.ka/actors.yml`)

```yaml
humans:
  bsant:
    name: Bryan Sant
    emails: [bryan.sant@example.com]
    aliases: [bryan.sant, "Bryan Sant"]      # used by `ka fix` and migration to normalize (L3)
agents:
  agent/claude:  { name: Claude Code }
  agent/codex:   { name: Codex }
roles:
  maintainers: [bsant]
```

- Agent actors default to `agent/<mcp clientInfo.name>`. Unknown agents are allowed and appear in the
  UI as unregistered.
- Every agent action records the **principal** (`on_behalf_of`): the human whose git identity or
  daemon login is running the agent.
- Without an actor registry, `ka` falls back to git `user.email` and `user.name`, normalized.

### 7.7 Boards (`boards/*.yml`)

A saved view: `{name, filter, group_by, columns, sort, swimlanes, card_fields}`. The filter language
is a small, documented query syntax shared by the UI, CLI, MCP, and REST (for example,
`project:jk,gyggly category:started -label:infra assignee:agent/*`). Boards are definitions only. They
never store ticket membership or column position (v0 lesson: shared reordered lists are worst-case
for merges). Manual ordering within a column is P2. If it is added, it will use a fractional-index
`rank` field on the ticket.

### 7.8 Code links

v0's symbolic references are kept, because they work: `repo://path[@ref][#Lstart[-end]]`.
`project.yml` declares a project's code repos:

```yaml
repos:
  jk: { forge: https://github.com/jkbuild/jk, default_branch: main }
```

Resolution is local-first: if the workspace knows a local checkout for `jk` (from config or from a
discovered `.ka.yml` pointer, §9.4), the link resolves to a file path. Otherwise it resolves to a
forge URL. Use a commit SHA for durable evidence and a branch name for "the current thing".

### 7.9 Commits and trailers

`ka` makes **one commit per operation**, with a readable subject and machine-readable trailers:

```
JK-7QK3M: in_progress → review

Ka-Op: transition status=review
Ka-Actor: agent/claude
Ka-On-Behalf-Of: bsant
Ka-Session: 01JC4N7Q2V8X
Ka-Spec: 1
```

- The **git author** is the principal's git identity. The actor is in the trailers.
- Trailers are **hints, not truth**. The activity feed is computed from the diff (frontmatter
  before and after, plus files added) and enriched with trailers when they are present. Hand-written
  batch commits produce correct events, just with less metadata (L7).
- `Ka-Op` makes commits **replayable** during rebase (§10.2).

### 7.10 Canonical formatting

- UTF-8, LF line endings, exactly one trailing newline. No BOM.
- Frontmatter keys appear in the order defined in §7.3. Lists use block style and are sorted
  (`labels`, `blocked_by`); `acceptance` keeps its order. Strings are quoted only when the YAML
  profile requires it.
- `ka fmt` (and `ka fix`) rewrite any file to canonical bytes. `ka check` reports non-canonical files
  as warnings, and as errors in CI mode.

### 7.11 Versioning and conformance

- `spec/v1/` holds the prose spec, JSON Schemas (`ticket.schema.json`, `comment.schema.json`,
  `config.schema.json`, `board.schema.json`, `actors.schema.json`), and a **conformance corpus**:
  valid and invalid store fixtures with expected `ka check` output.
- Spec changes that are backward compatible bump a minor version, which readers can ignore. Breaking
  changes bump `spec:`, and `ka migrate` upgrades a store in one commit.
- `ka` refuses to *write* to a store with a newer major spec than it supports. It can still read it,
  on a best-effort basis.

### 7.12 The hand-writing contract

`ka agents-md` generates the protocol section for `AGENTS.md`. The protocol is small enough to
follow by hand:

1. Pick a random 5-character suffix from `0-9A-HJKMNP-TV-Z`. Confirm `projects/<key>/tickets/<ID>/`
   does not exist.
2. Write `ticket.md` with the required fields (copy `ka template`'s output or the spec example).
3. To comment, add a new file under `comments/` named with a UTC timestamp. Never edit an existing
   comment.
4. To claim, set `claim` and `assignee`, commit, and push *before* starting work. If the push is
   rejected, pull with rebase and re-check.
5. Use `git pull --rebase`. Never create merge commits.

---

## 8. Architecture

```
            ┌──────────────── ka (single Go binary) ────────────────┐
 agents ──► │ MCP stdio (`ka mcp`) ─┐                               │
 agents ──► │ MCP HTTP  (/mcp) ─────┤                               │
 humans ──► │ Web UI (embedded) ────┤──► Service layer (ops, query) │
 scripts ─► │ REST /api/v1 + SSE ───┤        │            │         │
 humans ──► │ CLI / TUI ────────────┘        ▼            ▼         │
            │                       Store engine      Index + Event bus│
            │                (spec I/O, validation,  (in-memory, disposable;│
            │                 ops, merge, sync)      SSE fan-out, presence)│
            │                         │                              │
            └─────────────────────────┼──────────────────────────────┘
                                      ▼
                         system `git` (managed clones / attached checkouts)
                                      ▼
                               remotes (GitHub, GitLab, Gitea, bare repos, …)
```

### 8.1 Components

- **Spec I/O** (`internal/spec`): parsing and serializing ticket, comment, config, board, and actor
  files with the restricted YAML profile, schema validation, and canonical formatting. This is pure
  Go with no git dependency, and it is exported as a library (`pkg/kaspec`) so other tools can embed
  it.
- **Store engine** (`internal/store`): applies **operations** (create, update, claim, transition,
  comment, and so on) to a working tree, then commits, syncs, replays, and runs the merge driver.
  Each operation is a typed value that can be validated, applied, serialized into `Ka-Op`, and
  replayed.
- **Git layer** (`internal/git`): shells out to the **system `git` binary** (minimum version 2.40).
  Decision: system git rather than go-git, because go-git's rebase and merge support is incomplete,
  and system git gives us the user's credential helpers, SSH config, LFS, and performance for free.
  `ka doctor` checks the git version.
- **Index** (`internal/index`): an in-memory model of every ticket, comment, relation, and actor
  across the workspace. It is built at startup (target: 10,000 tickets in under 1 second) and updated
  incrementally from `git diff --name-status <old>..<new>` whenever HEAD moves. Full-text search
  starts as an in-memory inverted index. A persistent search index (for example, Bleve) can come
  later as a disposable cache.
- **Event bus** (`internal/events`): turns index changes into domain events (`ticket.updated` with a
  field diff, `comment.created`, `claim.changed`, `sync.state`, `presence.changed`) for SSE, MCP
  waiters, and `ka watch`.
- **Presence** (`internal/presence`): ephemeral and per-daemon. Tracks connected MCP sessions, the
  last tool call, and heartbeats. **Never written to git.** It is labeled as live state in the UI.
- **Transports**: MCP (stdio and Streamable HTTP, using the official Go MCP SDK), REST with an
  OpenAPI 3.1 spec (Huma or an equivalent code-first generator), SSE, and the embedded static UI.

### 8.2 Working trees: managed and attached

- **Managed (default):** `ka` clones each store into its data directory (`$XDG_DATA_HOME/ka/stores/`,
  `~/Library/Application Support/ka/` on macOS, `%LOCALAPPDATA%\ka\` on Windows). `ka` owns these
  clones: they are always clean, always rebased, and never touched by hand. This avoids fighting
  with a user's dirty checkout.
- **Attached:** point a store at an existing checkout (`ka store add --attach ./kanartist-data`).
  `ka` watches it with fsnotify. Hand edits appear as "uncommitted changes" in the UI and are never
  auto-committed. `ka` refuses operations while a rebase or merge is in progress, and tells you how
  to fix it.

### 8.3 Process model and concurrency

- **One writer per store per machine.** Every `ka` process takes a cross-platform file lock
  (`gofrs/flock`-style: `flock` on Unix, `LockFileEx` on Windows) on `<clone>/.git/ka.lock` for each
  write operation. Inside the daemon, writes to a store are serialized through a single goroutine
  queue.
- **Daemon discovery:** `ka daemon` writes a runtime file (`$XDG_RUNTIME_DIR/ka/daemon.json` or the
  OS equivalent) containing the port, PID, workspace hash, and a random local token. `ka mcp` and CLI
  commands **proxy to a running daemon** for the same workspace when there is one, which gives a
  shared index, presence, and live events. When there is no daemon, they run the engine in-process.
  Both paths have the same semantics.
- **Transport is TCP loopback only.** There are no Unix domain sockets or named pipes, so behavior is
  the same on every OS. The default bind is `127.0.0.1:7711` (configurable). `--listen 0.0.0.0` is
  for remote mode and requires auth (§15).

### 8.4 Repo layout of the `ka` project itself

```
ka/
├── cmd/ka/                 # main
├── internal/{spec,store,git,index,events,presence,mcp,api,cli,tui,auth,config}/
├── pkg/kaspec/             # public library: spec types, parse/format/validate
├── spec/v1/                # the Store Spec: prose, JSON Schemas, conformance corpus
├── web/                    # Angular app (built into internal/api/dist, embedded via go:embed)
├── docs/                   # PRD, architecture, user & agent guides
└── tools/migrate-v0/       # one-time v0 → v1 migrator (not shipped in the binary)
```

---

## 9. Workspaces and layering

### 9.1 Workspace definition

```yaml
# ~/.config/ka/workspace.yml   (or ./.ka-workspace.yml, or --workspace)
name: bryan
stores:
  - name: org
    url: git@github.com:JumpKickOSS/ka-org.git          # config, actors, cross-team boards
  - name: jk
    url: git@github.com:jkbuild/jk-tickets.git
  - name: gyggly
    url: git@github.com:Gyggly/gyggly.git
    branch: ka                                         # orphan-branch store inside the code repo
identity:
  actor: bsant
repos:                                                 # local checkouts for link resolution
  jk: ~/src/oss/jk
```

### 9.2 Store topology is flexible

A store holds **any number of projects** side by side under `projects/<key>/`. All of these are
valid, and the spec and the engine treat them the same way:

| Topology | Example |
|---|---|
| Everything in one store | `org` holds `config`, `actors`, `boards`, and the `jk`, `gyggly`, and `jcrm` projects |
| One store per project, plus an org store (**recommended practice**, and what the v0 migration produces) | `org` has config, actors, and boards. `jk-tickets` holds `jk`. `gyggly@ka` holds `gyggly`. |
| Grouped | `org`, `platform-tickets` (holds `jk` and `knest`), `apps-tickets` (holds `gyggly` and `jcrm`) |
| Single project, no org store | `nerfherdr@ka` holds `nerfherdr` and its own config |

Moving a project from one store to another (to split or combine) is supported by `ka project move
<key> --to <store>`. It copies `projects/<key>/` into the target store with one commit, then leaves a
`project.yml` tombstone (`moved_to_store:`) in the source store. Ticket IDs and paths inside the
project do not change, because paths are relative to the project.

- **Projects are a union across stores.** Each project lives in exactly one store, its *home*.
  Two stores declaring the same project key is an error that `ka check --workspace` reports. Having
  a single home avoids split-brain writes.
- Every operation writes to exactly one store, the home of the ticket being changed (principle 2).
- **Cross-store relations** (`blocked_by: GYG-K2M7P`) are resolved through the index. A reference to
  a project the workspace does not include is shown as *external* in the UI, not reported as an
  error.

### 9.3 Config layering: who overrides whom

**The problem with "workspace order":** a workspace is local. My list of stores and their order are
not the same as yours, and a CI job has its own. If validation depended on workspace order (whether
first wins or last wins), the same ticket could be valid on one machine and invalid on another.
That breaks decentralization. So config is split into three classes, each with its own resolution
rule.

#### Class 1: Data config (affects what counts as valid data)

This covers `workflow`, `kinds`, `labels`, `fields`, `id_suffix_length`, `claims.stale_after`,
size limits, and `template`. (Priorities are a fixed `P0`–`P3` vocabulary in the spec, not config.)

**This config is resolved along an explicit inheritance chain declared *in the stores
themselves*.** The workspace never takes part, so every client computes the same result for a given
ticket.

```yaml
# jk-tickets/.ka/store.yml
spec: 1
name: jk-tickets
extends:
  - url: git@github.com:JumpKickOSS/ka-org.git     # parent store (any number; order matters)
    ref: main                                    # branch, tag, or SHA (pin a SHA for full reproducibility)
```

For a ticket in project `P` with home store `S`, the effective data config is built by merging these
layers in order, with **later layers overriding earlier ones**:

```
built-in defaults
  → S's parents' configs (each parent's own chain resolved first, depth-first, left to right)
    → S/.ka/config.yml
      → S/projects/P/project.yml  (its `config:` block)
```

**The more specific layer wins** (the project beats its store, the store beats its parents),
**unless a parent locks a key** (see "Locks" below). This is how CSS, git config, and EditorConfig
behave, so it should be familiar.

- **Several parents** (`extends: [org, security-policy]`) merge left to right. The later parent
  overrides the earlier one, and the store overrides both. A store reached twice through different
  paths (a diamond) is applied once, at its first position. Cycles are an error. The maximum depth is
  4.
- **A store that holds several projects** applies its config to all of them. Each project can then
  override in its own `project.yml`.
- **A store with no `extends`** stands alone (built-in defaults, then itself). The single-store and
  single-project topologies need nothing more.
- **An org store can also host projects.** It is both a parent of other stores and a home for its
  own projects. Nothing special is required.

#### Merge rules (same for every layer)

| Value type | Rule | Example |
|---|---|---|
| Scalar | The later layer replaces it | `id_suffix_length: 6` |
| Map | Deep merge key by key. Setting a key to `null` deletes the inherited key. | `fields:` (org declares `estimate`; jk adds `jk_module` and sets `estimate: null`) |
| **Keyed collection** (`fields`, actors) | Merged by key, like a map | |
| **Ordered list** (`workflow.statuses`, `kinds`, `labels.known`, `template.sections`) | **Replaced wholesale.** A partially merged workflow is a trap. To change one, restate the whole list. | |
| List with explicit patch | `kinds: { $append: [spike] }` or `{ $remove: [perf] }`. Only for simple string lists. | jk adds `spike` without restating org's kinds |

#### Locks

A parent can stop child layers from overriding particular keys:

```yaml
# ka-org/.ka/config.yml
locked: [workflow, kinds, fields.security_review]
```

A child that overrides a locked key gets a `ka check` **error** (`config_locked: workflow is locked
by ka-org`), and the parent's value is used. Locks pass down the whole chain. Only the store that set
a lock can remove it.

Locks are the **only** constraint on children (D12). A key that is not locked can be overridden in
either direction: a child may tighten a parent's vocabulary (close `labels`, remove a kind) or loosen
it (open `labels`, add kinds). An org that wants to forbid loosening locks the key.

#### When a parent is unavailable

`ka` keeps a managed clone of every store in a chain, even ones not listed in the workspace, because
they are fetched automatically. If a parent cannot be reached:

- **Reads** work, using the last cached copy of the parent's config and a visible
  `config_stale` warning.
- **Writes** use the cached copy if one exists and record its commit in the `Ka-Config` trailer. If
  no copy was ever fetched, writes to that store are refused with a clear error (`parent store
  ka-org unreachable and never fetched; run ka sync or check access`). Writing data validated against
  the wrong rules would be worse than not writing.
- People without read access to a parent store (for example, a contributor to a public project
  whose org config is private) can't fully validate it. The store's owner can avoid this by making
  config parents readable to everyone who can read the store, or by not using `extends`. `ka check`
  reports the situation explicitly.

#### Class 2: Directory data (actors, boards)

These are things to look up, not validation rules. Unknown actors are allowed (§7.6), so they don't
need a deterministic chain.

- **Actors:** a **union across every store in the workspace plus every store's extends chain**.
  If two stores define the same handle differently, the definition from the ticket's own home chain
  is used when displaying that ticket's data. Otherwise the first in workspace order wins, with a
  `ka check --workspace` warning. **Recommended practice:** define actors once, in the org store.
- **Boards:** a union. Every board from every store is available. Board names are namespaced by
  store (`org/agents-in-flight`) in the UI, so they never collide.

#### Class 3: Runtime config (how this `ka` instance behaves)

This covers `sync.push`, `sync.fetch_interval`, daemon listen address, UI defaults, identity, and
link-resolution paths. It affects behavior, never validity. It is resolved with **the user
winning**:

```
built-in defaults → store's recommended runtime block (`runtime:` in .ka/config.yml) → workspace.yml → environment variables → command-line flags
```

A store's `runtime:` block holds recommendations only (for example, "please debounce pushes"). A
store can raise a recommendation into a minimum (`runtime.enforce: { sync.fetch_interval: ">=10s" }`)
to protect a shared remote from fetch storms. That is the only way a store can constrain a client's
runtime settings.

#### Seeing where a value came from

`ka config explain [--project jk] [key]` prints the effective value of each key and the layer it came
from, plus any locks:

```
workflow.statuses   ka-org/.ka/config.yml@4e1a9c2   (locked by ka-org)
kinds               jk-tickets/.ka/config.yml        ($append: spike onto ka-org)
fields.estimate     <deleted by> jk-tickets/projects/jk/project.yml
sync.push           ~/.config/ka/workspace.yml       (store recommends debounce:2s)
```

The same information is available through MCP (`ka_schema` includes the source of each value when
asked) and in the UI's project settings page, which is read-only in v1 because config changes are
ordinary commits.

### 9.4 Discovery from a code repo

A code repo can contain a tiny, rarely changing pointer file, `.ka.yml`:

```yaml
store: git@github.com:jkbuild/jk-tickets.git
project: jk
```

When run anywhere inside that checkout, `ka` (CLI or `ka mcp`) walks up the tree and finds the
pointer. It adds the store to an implicit workspace (cloning a managed copy if needed), uses `jk` as
the default project, and registers the checkout as the local path for `jk://` links. **An agent
that starts in a code repo can run `ka next` with no setup.** This replaces v0's prose stanza in
`AGENTS.md` with a pointer machines can read. (`ka agents-md` still generates a short human-readable
stanza.)

---

## 10. Sync and conflict resolution

The rules are opinionated. The goal is that conflicts essentially never reach a human, and nothing is
ever lost.

### 10.1 Rules

1. **Linear history.** `ka` only fast-forwards or rebases, and never creates merge commits. `ka
   check` warns on merge commits in a store's history (hand pushes are tolerated).
2. **One operation per commit, pushed early.** By default each operation pushes immediately.
   `runtime.sync.push: debounce:2s` batches bursts. **Claims always push synchronously** and are reported as
   unconfirmed until the push succeeds.
3. **Fetch often.** The daemon fetches every `fetch_interval` (15 seconds by default), and
   immediately when a forge webhook arrives at `POST /api/v1/hooks/git` or when the UI or an agent
   asks for fresh data.
4. **Push retries are bounded.** On rejection: fetch, rebase, push, up to 8 attempts with jittered
   exponential backoff. If all attempts fail, the store enters `sync: degraded`. This is visible in
   the UI, and operations keep committing locally.

### 10.2 Replaying operations (the main mechanism)

When a rebase hits a conflict on a commit that `ka` made (it has a `Ka-Op` trailer), `ka` does not
merge the text. Instead it:

1. drops the local commit,
2. re-reads the upstream state of the affected file,
3. **re-checks the operation's preconditions** against that state (for example, a claim requires the
   ticket to be unclaimed or claimed by this session; a transition requires the ticket to exist and
   not be tombstoned), and
4. re-applies the operation and commits it again.

If the preconditions now fail (for example, someone else claimed the ticket first), the operation
returns a structured error to its caller (`claim_lost`, with the holder's details). This generalizes
v0's claim protocol to every operation.

### 10.3 Semantic merge driver (for commits made outside `ka`)

`.gitattributes` maps `ticket.md` to `merge=ka`. `ka init` and `ka doctor` register the driver
(`git config merge.ka.driver "ka merge-driver %O %A %B %P"`). If the driver is not registered, git
falls back to its ordinary text merge, which is harmless. The driver merges frontmatter **field by
field** with a 3-way merge:

- **Field changed on only one side:** take that side's value.
- **Set fields** (`labels`, `blocked_by`, `related`): set-wise 3-way merge (additions and removals
  from both sides).
- **`acceptance`:** merged per item, matched by text. A `done: true` from either side wins.
- **Scalar changed differently on both sides:** the upstream (already published) value wins. The
  losing local value is preserved (§10.4).
- **`updated`:** the maximum of both sides.
- **Body:** git's normal text merge. If the bodies conflict textually, the upstream body wins and the
  local body is preserved (§10.4).

### 10.4 No silent loss

Any value that loses a merge is written as a `system` comment on the ticket. The comment contains
the losing value or body, its author, and its commit. A `sync.conflict_preserved` event is emitted,
which the UI shows. The merge always completes. A human or agent can restore the preserved content
with one action.

### 10.5 Repair tools

- **`ka sync`:** fetches, rebases, replays operations, and pushes. Prints a summary of what happened.
- **`ka doctor [--fix]`:** checks the git version, merge-driver registration, `.gitattributes`,
  managed clone health (rebase in progress, detached HEAD, dirty tree), lock files left behind by
  dead processes, and clock skew.
- **`ka fix`:** normalizes data (canonical formatting, CRLF, actor aliases, missing timestamps,
  duplicate unpublished IDs). Its output is one commit per fix category.

---

## 11. MCP server

The MCP server is the primary interface, and the product is designed around it.

### 11.1 Design rules

- **Few, coarse tools.** About 14 tools, each covering a complete intent. No chatty CRUD sequences.
- **Dynamic schemas.** Tool input schemas include the workspace's actual enums (statuses, kinds,
  labels if closed, custom fields), so agents cannot invent values (L4) and do not need to read docs
  to learn the vocabulary.
- **Compact output.** Every response returns `structuredContent` plus a short text rendering
  optimized for LLMs. List tools accept `fields` and `limit` and default to summaries. `get` returns
  the description plus a *digest* of comments (last N, open questions, pinned decisions), not the
  whole thread, unless asked.
- **Actionable errors.** Every error carries a code, a one-line message, and a `fix` hint (for
  example, `unknown_status: "doing" is not a status in project jk. Did you mean in_progress?`).
- **Batching.** `create` and `update` accept arrays and apply them in one commit per ticket, with
  one push.
- **Identity is automatic.** Actor = `agent/<clientInfo.name>`. Session = the MCP session ID.
  Principal = the local git identity or the daemon login.

### 11.2 Tools (v1)

| Tool | Purpose |
|---|---|
| `ka_next` | Return the best ticket to work on (filters: project, kind, labels). Default `claim: true` claims it atomically. Returns the ticket plus a context digest (parent, blockers' status, linked docs, open questions). |
| `ka_search` | Query using the shared filter language, with full text, `fields`, `sort`, and `limit`. |
| `ka_get` | Full ticket. `include: [comments, history, children, relations]`. |
| `ka_create` | Create one or many tickets from structured fields (`title`, `kind`, `priority`, `problem`, `approach`, `acceptance[]`, `parent`, `blocked_by`, `labels`, `fields`). Returns IDs. |
| `ka_update` | Patch fields of one or many tickets (`set`, `add`, `remove` for set fields). |
| `ka_claim` / `ka_release` | Explicit claim (with `steal_stale`) and release (with target status). |
| `ka_transition` | Change status. Moving to a `completed` or `canceled` category requires a `resolution` and a summary, which are written as a resolution comment. |
| `ka_comment` | Add a `note`, `progress`, or `decision` comment. |
| `ka_check` | Mark acceptance items done or not done (by index or text). |
| `ka_ask` | Ask a human: `{question, options?, to?, kind: question\|approval}`. Optional `wait: <duration>` blocks until an answer arrives or the timeout expires (backed by the event bus, or by fetch polling without a daemon). |
| `ka_inbox` | For agents resuming work: answers and approvals received on their tickets since a given time. |
| `ka_schema` | Describe the workspace: projects, workflows, kinds, labels, fields, actors. Cached by agents. |
| `ka_sync` | Report or force the sync state, and list preserved conflicts. |
| `ka_resolve_link` | Turn a `repo://` reference into a local path or forge URL. |

MCP **resources** (`ka://ticket/JK-7QK3M`, `ka://board/agents-in-flight`) and **prompts**
(`ka/plan-epic`, `ka/write-ticket`) are provided for clients that support them. Tools are the
contract, because many clients support only tools.

### 11.3 Token budget targets

| Operation | Target (tool call plus response, approximate tokens) |
|---|---|
| `ka_next` with claim and context digest | ≤ 800 |
| `ka_transition` to done with resolution | ≤ 150 |
| `ka_comment` (progress) | ≤ 100 |
| `ka_search` returning 20 summaries | ≤ 1,200 |

These are measured in CI with a tokenizer harness against fixture stores, and treated as regression
budgets.

---

## 12. REST API and SSE

### 12.1 REST (`/api/v1`)

- Resources: `workspace`, `stores`, `projects`, `tickets`, `tickets/{id}/comments`, `boards`,
  `actors`, `inbox`, `activity`, `sync`, `search`.
- It uses the same service layer as MCP, so behavior cannot diverge. An OpenAPI 3.1 document is
  served at `/api/v1/openapi.json`, and the Angular client is generated from it.
- Writes use `PATCH` with field operations. Body edits require `If-Match: <blob sha>` to prevent lost
  updates in the UI's editor. Field patches do not need it, because the operation is replayable.
- Errors use `application/problem+json` with the same codes as MCP.

### 12.2 SSE (`GET /api/v1/events`)

- Event types: `ticket.created`, `ticket.updated` (with a field diff and the new summary),
  `ticket.moved`, `comment.created`, `claim.changed`, `question.opened`, `question.answered`,
  `presence.changed`, `sync.state`, `sync.conflict_preserved`, `store.changed`, `reset`.
- Payloads are self-sufficient for updating the UI, so no refetch is needed.
- `id:` is a monotonic daemon sequence number. The daemon keeps a ring buffer (10,000 events by
  default). A client reconnecting with `Last-Event-ID` gets replayed events. If the ID is too old,
  it gets a `reset` event and refetches its snapshot.
- Filter with query parameters (`?projects=jk,gyggly&types=ticket.*`). The server sends a heartbeat
  comment every 15 seconds so proxies keep the connection open.
- Latency targets: an operation made through this daemon reaches clients in under 250 ms. A push
  from elsewhere arrives within one fetch interval, or under 2 seconds when webhooks are configured.

---

## 13. Web UI

### 13.1 Look and feel

A cross between **Linear** (density, speed, keyboard-first, dark-by-default polish, peek panels,
command palette) and **GitHub Projects** (flexible board, table, and roadmap views over a shared
query, grouping, custom-field columns). It should feel calm and fast, and it should be beautiful to
leave open all day.

- Angular (latest stable major; decision D7), standalone components, signals, zoneless,
  strict TypeScript. The production bundle is embedded in the Go binary. No CDN dependencies, so it
  works offline.
- Dark and light themes, compact and comfortable density, and a design-token-based theme.
- **Movement is meaningful.** When an agent moves a card, it animates to its new column and briefly
  pulses with the actor's avatar. Changes from other people get a subtle highlight, never a modal.
- Agents are visually distinct from humans (a robot glyph and a distinct avatar ring) everywhere an
  actor appears.

### 13.2 Views

| View | Priority | Description |
|---|---|---|
| **Activity** | P0 | The main "watch" view. A live, grouped stream of events across the workspace ("agent/claude moved JK-7QK3M to review: 'processor marked non-incremental'"). Filter by project, actor, or type. Click any event to see the ticket and the git diff behind it. |
| **Board** | P0 | Kanban columns by status (or any grouping), swimlanes (project, assignee, priority, parent), and cards showing ID, title, priority, kind, labels, assignee, claim age, acceptance progress, and blocked state. Drag and drop is a P1 human interaction. |
| **Ticket peek and detail** | P0 | Linear-style side panel: frontmatter as editable properties, rendered description, acceptance checklist, relations graph (parent, children, blockers), and a **timeline** that merges comments with git-derived field-change events. Each event links to its commit. |
| **Agents** | P0 | Who is working on what: live sessions (presence), held claims, claim age, staleness warnings, last action, and throughput per agent today. One-click "release stale claim". |
| **Inbox** | P0 | Open questions and approval requests addressed to me or my roles, newest first. Answer inline (option buttons or free text). A badge count appears in the shell and the browser tab title, with optional browser notifications. |
| **List / Table** | P1 | GitHub Projects-style table with configurable columns (including custom fields), group-by, sort, and inline edits. |
| **Saved boards** | P1 | Render `boards/*.yml`. Create and edit views and save them back to a store (committed like everything else). |
| **Docs** | P1 | Browse and render `projects/<key>/docs/`, with backlinks from tickets. |
| **Sync & health** | P1 | Per-store sync state, last fetch and push, degraded mode, preserved conflicts with a "restore" action, and `ka check` findings. |
| **Roadmap / milestones** | P2 | Milestones with progress, and a timeline over `due` and milestone dates. |

### 13.3 Global UX

- **Command palette (Ctrl/⌘-K):** jump to a ticket by ID or title, run actions, switch views.
- **Keyboard shortcuts** for everything, following Linear's conventions (`c` create, `s` status,
  `p` priority, `a` assign, `j/k` move, `/` search).
- Every ticket and view has a deep link. `ka open JK-7QK3M` opens it in the browser.
- Human writes are attributed to the logged-in actor and committed by the daemon like any other
  operation.
- Accessibility: WCAG 2.2 AA. All drag-and-drop actions have keyboard equivalents.

---

## 14. CLI and TUI

The CLI is intentionally small. It exists for (a) agents that have a shell but no MCP, (b) humans
running a precise command an agent recommended, and (c) operations and repair. Every read command
supports `--json`. When arguments are missing and stdin is a TTY, commands prompt interactively
(Charm `huh`-style forms). `ka watch` is the only full-screen TUI.

| Command | Purpose |
|---|---|
| `ka init [--project <key>]` | Create a store in the current repo or branch (store.yml, config, .gitattributes, AGENTS.md stanza) and register the merge driver. |
| `ka daemon [--listen] [--open]` | Run the web UI, REST/SSE API, and MCP over HTTP. |
| `ka mcp` | MCP over stdio. Proxies to the daemon if one is running. |
| `ka open [<id>\|<view>]` | Open the UI in a browser at a ticket or view. |
| `ka next [--claim]`, `ka ls <query>`, `ka show <id>` | Find and read. |
| `ka new`, `ka edit <id> --set k=v`, `ka claim`, `ka release`, `ka move <id> <status>` | Write. `ka edit <id>` with no flags opens `$EDITOR` and validates on save. |
| `ka comment <id> [-m]`, `ka answer <id> [--choice N]`, `ka approve <id>`, `ka reject <id>` | Human-in-the-loop actions (scenario S9). |
| `ka watch [query]` | Live activity stream in the terminal (TUI). |
| `ka sync`, `ka doctor [--fix]`, `ka check [--ci]`, `ka fix`, `ka fmt` | Sync, health, validation, repair. |
| `ka store add\|rm\|ls`, `ka workspace` | Workspace management. |
| `ka config explain [--project] [key]` | Show effective config values, the layer each came from, and locks (§9.3). |
| `ka project move <key> --to <store>` | Move a project between stores (split or combine, §9.2). |
| `ka agents-md`, `ka template` | Generate the hand-writing protocol and a blank ticket. |
| `ka migrate` | Upgrade the store spec version. |
| `ka version`, `ka completion <shell>` | |

---

## 15. Identity, authentication, and security

- **Local mode (default):** the daemon binds to loopback. The principal is the local git identity,
  mapped through the actor registry. Browser access requires the random local token, which `ka open`
  passes once and the daemon exchanges for an HttpOnly cookie. The daemon enforces `Host` and
  `Origin` checks to prevent DNS rebinding and CSRF. MCP over HTTP requires the bearer token from
  the runtime file.
- **Remote mode:** binding to a non-loopback address requires auth to be configured. v1 supports:
  1. **Static API tokens** (`ka token create --actor bsant`), stored hashed in the daemon's local
     config, *not* in git.
  2. **Trusted reverse-proxy headers** (for example, oauth2-proxy or Cloudflare Access), with an
     allow-list of proxy IPs.
  3. OAuth 2.1 for MCP over HTTP, following the MCP authorization spec. P2.
- **Write authority** in v1 is "anyone authenticated to the daemon can write, using the daemon's git
  credentials". Fine-grained permissions are a non-goal. Forge branch protection still applies.
- **Secrets:** stores must never hold secrets. `ka check` runs a lightweight secret scanner (common
  token patterns) and warns. Attachments are size-capped.
- **Untrusted content:** ticket and comment text written by other agents is data. The MCP server
  marks it as such in text renderings. This is part of the agent guide, and it is a mitigation, not
  a guarantee.
- **Supply chain:** releases are reproducible, signed (cosign or Sigstore), checksummed, and ship
  with an SBOM.

---

## 16. Validation, CI, and hooks

- **`ka check`** covers schema validity, unique IDs (within a store and across the workspace),
  directory and ID agreement, references to tickets that do not exist (internal only; external ones
  are informational), cycles in `parent` and `blocked_by`, unknown keys, undeclared custom fields,
  invalid enums, non-canonical formatting, CRLF, placeholder text, stale claims (informational),
  orphaned comments, low-entropy IDs, secrets, and file size limits.
- **`ka check --ci`** turns warnings into errors where appropriate and outputs GitHub annotations,
  SARIF, or JSON.
- **GitHub Action** (`kanartist/check-action`) downloads the release binary and runs
  `ka check --ci`. It is documented for GitLab CI and generic CI too.
- **Optional client hook:** `ka hooks install` adds a `pre-commit` hook running `ka check --staged`
  and `ka fmt --staged`. This catches mistakes in hand-written files *before* the push (L4).

---

## 17. Non-functional requirements

| Area | Requirement |
|---|---|
| Install | Single static binary (CGO disabled). Requires only `git` 2.40 or later. Under 1 minute from zero to `ka next`. |
| Platforms | linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64, windows/arm64. CI runs the full test suite on all three OS families. |
| Scale (per store) | 50,000 tickets, 500,000 comments, 100 concurrently active agents. Workspace: 20 stores. |
| Performance | Cold index of 10,000 tickets in under 1 s. Warm query in under 20 ms. Local operation (commit, without push) under 100 ms p95. SSE fan-out under 250 ms. Memory under 300 MB at 50,000 tickets. |
| Reliability | No operation can leave a managed clone corrupted. If interrupted, the next start recovers (aborts a half-finished rebase, replays operations). Fuzz tests on the parser and merge driver. |
| Correctness | Property tests: for random concurrent operation streams across N simulated clones, the final state equals some serial order of accepted operations, IDs are unique, and every rejected or overwritten value is preserved. |
| Offline | Every operation works offline (claims are marked unconfirmed). Sync resumes when the network returns. |
| Observability | Structured logs (`slog`), `/healthz`, `/metrics` (Prometheus) in daemon mode. |
| Accessibility | WCAG 2.2 AA for the web UI. |

---

## 18. Distribution and open-source project

- **Releases:** GoReleaser publishes to GitHub Releases, a Homebrew tap, Scoop and winget,
  `.deb`/`.rpm`, a container image (`ghcr.io/…/ka`) for remote daemons, and `go install`.
- **Agent onboarding in one line,** documented per client (for example, `claude mcp add ka -- ka mcp`).
  A Claude Code plugin or skill bundling the MCP config and agent guide is P1.
- **Docs site:** getting started, agent guide, Store Spec, conformance, operations, and
  contributing.
- **License:** Apache-2.0 (decision D6).
- **Governance:** spec changes go through an RFC-style issue process. The conformance corpus is part
  of the spec.
- **Dogfooding:** KanArtist's own tickets live in a KanArtist store from the first milestone.

---

## 19. Success metrics

| Metric | Target |
|---|---|
| Share of agent ticket operations made through `ka` (commits with `Ka-Op` trailers) rather than by hand | > 95% within 1 month of adoption (v0 has a visible mix of the two) |
| Duplicate IDs in published history | 0 |
| Conflicts needing human intervention | < 1 per 10,000 operations |
| Silently lost edits | 0 (every merge loss is preserved, §10.4) |
| Median tokens per common agent action | Within the §11.3 budgets. At least 50% fewer than hand editing in a scripted benchmark. |
| Install success on a clean machine (3 OSes, scripted) | 100% |
| Time from an agent asking a question to the human seeing it (local daemon) | < 2 s |

---

## 20. Migrating from v0 (one-time, manual)

v0 compatibility is an anti-goal. Migration is a throwaway tool (`tools/migrate-v0`, not in the
binary) run once, after which the v0 repo is archived read-only.

1. **IDs:** every `JK-4661` gets a new random ID. The old ID is kept as a declared custom field
   (`fields.legacy_id: JK-4661`). It is searchable, and `ka show JK-4661` resolves it through an
   index alias. The duplicate `JK-4663` pair simply becomes two distinct tickets.
2. **References:** `depends_on`, and `JK-NNNN` mentions in bodies, are rewritten to the new IDs
   (`depends_on` becomes `blocked_by`).
3. **Body history:** sections such as `## Outcome`, `## Resolution`, `## Done`, `## Closing note`,
   `## Shipped`, and `## Closed by audit (…)` are pulled out of bodies into `resolution` or `note`
   comments, dated where the heading has a date and otherwise dated by the ticket's `updated`. Empty
   template placeholder sections are removed. `## Acceptance` checklists move into the `acceptance`
   frontmatter field.
4. **Normalization:** CRLF becomes LF. Assignees are mapped through an alias table (`bryan.sant`,
   `"Bryan Sant"`, and `bsant` become `bsant`; `claude` becomes `agent/claude`; `unknown` is
   dropped). The invalid `kind` on JK-2008 is fixed by hand. `go-do` becomes `feature`. Extension keys
   (`branch`, `source`, `estimate`, `order`, `shipped`, `status_note`, …) become declared custom
   fields or comments.
5. **Timestamps:** v0 dates become midnight UTC, and `claimed_at` becomes `claim.at`. Claims on
   `done` tickets are dropped. Open `in_progress` claims become stale claims.
6. **Archive:** `projects/gyggly/archive/2026/*` is flattened into `tickets/`. The `gyggly-java`
   project is kept, with an `archived: true` project flag (read-only in the UI; `ka` refuses new
   tickets).
7. **Docs:** `projects/<key>/docs/` carries over unchanged.
8. **History:** v1 stores start with fresh history and a single import commit. The v0 repo stays
   available as the archive of historical activity (decision D4).
9. **Verify:** run `ka check --ci` on the result. A spot-check report lists counts per project,
   status, and kind before and after.
10. **Retire v0:** delete the Python package, `bin/ka`, `requirements.txt`, `schema/`, and the CI
    workflow from the v0 repo, and add a README pointer to the new store.

Splitting stores: migration produces one store per v0 project (`jk`, `gyggly` together with the
archived `gyggly-java`, `jcrm`, `knest`, and `nerfherdr`) plus an `org` store for actors, the shared
workflow, kinds, custom fields, and cross-project boards. Each project store has
`extends: [ka-org]`. This exercises layering from day one (decision D5). The migrator also accepts
`--single-store` to put everything in `org`, which proves the other topology against the same
data.

---

## 21. Milestones

| Milestone | Scope | Exit criteria |
|---|---|---|
| **M0: Spec** | `spec/v1` prose, JSON Schemas, conformance corpus. `pkg/kaspec` parse, format, and validate. `tools/migrate-v0` run against the real corpus. | All 3,959 v0 tickets migrate and pass `ka check`. The spec is reviewed and frozen at `1.0-rc`. |
| **M1: Engine + CLI + MCP stdio** | Store engine, operations, managed clones, sync with operation replay, merge driver, index. CLI (core commands). `ka mcp` with all §11.2 tools. Cross-OS CI. | Scenarios S1, S4, S5, S6, S8, S9 pass as automated tests (S5 in the multi-clone simulator). Token budgets met. KanArtist dogfoods itself. |
| **M2: Daemon + live UI (watch)** | `ka daemon`, REST, SSE, MCP over HTTP, presence. Angular shell, Activity, Board (read-only), Ticket peek, Agents, Inbox (with answering). | S2 and S3 pass end to end. Latency targets met. Used daily on the migrated stores. |
| **M3: Interaction + layering** | Human editing (drag and drop, properties, create, body editing with If-Match), List/Table, saved boards, Docs, Sync & health view, multi-store workspaces, `.ka.yml` discovery, `extends` config layering with locks, `ka config explain`, `ka project move`. | S7 passes, on both the split and the single-store migration output. A non-author team installs it from the docs alone. |
| **M4: Remote + release 1.0** | Remote mode auth (tokens, proxy headers), webhooks, container image, packaging (brew, scoop, winget, deb/rpm), signed releases, docs site, GitHub Action. | Public 1.0 release. A remote daemon serves a team while agents on 3 OSes contribute. |
| **Later** | Milestones and roadmap, manual ranking, MCP OAuth, persistent search index, one-way importers (GitHub Issues, Jira, Linear), notifications (email, Slack, webhooks out), federated presence between daemons, forge PR auto-linking. | |

---

## 22. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Push contention under many agents causes rebase storms | Small commits, operation replay (cheap), jittered backoff, `debounce` push mode, a single writer per machine through the daemon. Simulated in property tests at 100 agents. |
| Repo growth from per-operation commits (v0: 8,455 commits in about 4 months) | Git handles millions of small commits well. Measure clone size at 1 million operations in M1. Recommend `--filter=blob:none` partial clones for large stores. Allow progress-comment throttling per config. |
| Agents still write files by hand | Spec designed for hand-writing (random IDs, append-only comments). `ka fix`, the pre-commit hook, and CI check. A `.ka.yml` pointer makes `ka` discoverable. Measured by the `Ka-Op` share metric. |
| Presence is per-daemon and not shared | Labeled as live, non-authoritative state. Durable signals (claims, progress comments) are in git. Federation is listed for later. |
| Forge rate limits on frequent fetches | 15 s default with backoff on errors. Webhooks are preferred for remote daemons. Fetches are cheap when there is nothing new. |
| Restricted YAML profile conflicts with editors and other tools | It is a strict subset of YAML 1.2, so any YAML parser reads it. Only the *writer* is restricted. |
| Windows quirks (paths, locks, CRLF, antivirus slowing git) | Windows in CI from day 1. `.gitattributes` enforced. Short paths. Lock abstraction. `ka doctor` checks `core.autocrlf` and long-path settings. |
| Prompt injection through ticket content | Treat it as data (§15). The agent guide explicitly covers it. No tool performs actions based on ticket text alone. |
| Scope creep toward a human-centric Jira | Principle 8 and the non-goals list. Every UI feature must serve watching or targeted intervention. |

---

## 23. Decisions (all resolved 2026-11-07)

| # | Decision | Outcome |
|---|---|---|
| D1 | Ticket ID format | **Random Crockford base32 suffix, 5 characters by default** (§7.1). |
| D2 | Default status vocabulary | **The v0 names** (`backlog`, `ready`, `in_progress`, `blocked`, `review`, `done`) **plus `triage` and `canceled`**, mapped to the fixed categories (§7.5). |
| D3 | Should `ka_next` claim by default? | **Yes.** `claim: false` peeks without claiming. |
| D4 | Replay v0 git history or start fresh | **Start fresh.** The v0 repo is archived read-only as the historical record. |
| D5 | Store topology for the migrated content | **One store per project plus an `org` store.** This is the recommended practice, not a requirement. Any topology (all in one store, grouped, split) is valid (§9.2), and config layers through explicit `extends` chains (§9.3). |
| D6 | License | **Apache-2.0.** |
| D7 | Angular version | **Latest stable major**, tracked through M2. Standalone, signals, and zoneless are required. |
| D8 | Progress comments: git or ephemeral? | **Git**, with per-config throttling. Token-level live streams stay ephemeral. |
| D9 | Where do mature docs live? | **v0's graduation rule stays as guidance.** Canonical docs move to the code repo, and the ticket links them SHA-pinned. The UI supports both. |
| D10 | Product and binary name | **KanArtist / `ka`.** Check package registries for conflicts before 1.0. |
| D11 | Should `extends` follow a branch or require a pinned SHA? | **Branch allowed** (follow the parent live). A SHA or tag can be pinned for reproducibility. `ka check --ci` can require pins through a config flag. |
| D12 | Can a child loosen a parent's closed vocabularies when the key is not locked? | **Yes.** Only `locked:` constrains children. |

---

## Appendix A: Prior art

| System | What we take | Where we differ |
|---|---|---|
| **Linear** | Visual and interaction quality, workflow categories, keyboard-first, peek panel | Agent-first. Git as storage. Decentralized. |
| **GitHub Projects** | Views over a shared query, table/board/roadmap, custom fields, cross-repo aggregation | Not tied to a forge. Works offline. Git-native data. |
| **Jira** | Breadth of concepts (relations, resolutions, custom fields) | No server database. No admin overhead. |
| **git-bug** | Git as a decentralized issue store, offline-first, Go | Plain files in a normal tree (browsable, hand-writable) instead of custom refs and objects. |
| **Backlog.md and similar markdown task tools** | Markdown tasks in git, CLI, MCP, web board | A multi-store layered workspace, a sidecar-first design, a formal spec and conformance suite, a conflict engine, and a live-activity focus. |
| **KanArtist v0** | Frontmatter tickets, a closed vocabulary, push-as-lock claims, symbolic code links, generated views | Everything in §2. |

## Appendix B: Requirement traceability (v0 lessons to sections)

L1 → §7.1 IDs, §10.2 · L2 → §7.10, §10.5 · L3 → §7.6, §20 · L4 → §11.1, §16 · L5 → §7.3
`acceptance`, §11.2 `ka_create` · L6 → §7.4 · L7 → §7.9 · L8 → §7.3 `claim`, §7.5 `claims` · L9 →
§7.2 · L10 → §7.5 `fields` · L11 → §8, §17, §18 · L12 → §7.2 `docs/`, §13.2 · L13 → §7.3, §16
