# KanArtist Store Spec, version 1

**Status:** 1.0-rc (M0)
**Schemas:** [`schema/`](schema/) · **Built-in defaults:** [`defaults.yml`](defaults.yml) · **Conformance corpus:** [`conformance/`](conformance/)

This document defines how KanArtist data is laid out in a git repository. It is the real
interface of the system: `ka` is one implementation, and any tool (or agent with a text editor)
that reads and writes files as described here is a valid KanArtist client.

The key words MUST, MUST NOT, SHOULD, SHOULD NOT and MAY are used as in RFC 2119. Where this
document and the conformance corpus disagree, the corpus wins and this document has a bug.

Contents:
1. [Concepts](#1-concepts) ·
2. [Layout](#2-layout) ·
3. [File format](#3-file-format) ·
4. [Tickets](#4-tickets) ·
5. [Store files](#5-store-files) ·
6. [Config and layering](#6-config-and-layering) ·
7. [Comments](#7-comments) ·
8. [Relations and links](#8-relations-and-links) ·
9. [Validation](#9-validation) ·
10. [Commits](#10-commits) ·
11. [Writing by hand](#11-writing-by-hand) ·
12. [Versioning](#12-versioning)

---

## 1. Concepts

- A **store** is a directory in a git repository (the repository root, or a subdirectory, on any
  branch) containing `.ka/store.yml`.
- A store holds any number of **projects**. A project has a unique **key** (`jk`) and a unique
  ID **prefix** (`JK`). Each project lives in exactly one store, its *home*.
- A project holds **tickets**. A ticket is a directory containing `ticket.md` (its current state)
  and optionally append-only **comments** and attachment **files**.
- A **workspace** is a set of stores used together. Workspaces are local and are not described by
  any file in a store; they never affect whether data is valid (§6.1).
- **Config** controls vocabularies (statuses, kinds, labels, custom fields) and limits. It is
  inherited through stores' explicit `extends` chains (§6).

## 2. Layout

```
<store root>/
├── .ka/
│   ├── store.yml          REQUIRED. Marks the store (§5.1)
│   ├── config.yml         optional. Data config (§6)
│   └── actors.yml         optional. Actor registry (§5.4)
├── .gitattributes         RECOMMENDED (§3.4)
├── boards/<name>.yml      optional. Saved views (§5.5)
└── projects/<key>/
    ├── project.yml        REQUIRED for each project (§5.2)
    ├── docs/              optional. Free-form project documents
    └── tickets/<ID>/
        ├── ticket.md      REQUIRED (§4)
        ├── comments/<yyyymmddThhmmssZ>-<XXXX>.md   optional (§7)
        └── files/         optional. Attachments, any layout
```

Rules:

1. A ticket's path MUST be `projects/<key>/tickets/<ID>/ticket.md`, where `<key>` is its
   project's key and `<ID>` equals the ticket's `id` field. Paths never change: there is no
   archive directory, and tickets are never renamed or moved within a store.
2. Inside a project directory only `project.yml`, `tickets/` and `docs/` are allowed. Inside a
   ticket directory only `ticket.md`, `comments/` and `files/` are allowed. Inside `comments/`
   only comment files are allowed. Anything else is a `stray_file` warning.
3. `docs/` content is not interpreted. Files elsewhere in the store (README, CI config, …) are
   ignored.
4. Names MUST be portable: ASCII, no `:`, no names differing only by case, and paths short enough
   for Windows (keep the full path well under 260 characters).
5. `milestones/` at the store root is reserved for a future minor version.

## 3. File format

### 3.1 Documents

`ticket.md` and comment files are **documents**: a YAML frontmatter block followed by a Markdown
body.

```
---
<frontmatter: a YAML mapping>
---

<body: CommonMark/GitHub-flavored Markdown>
```

The first line MUST be exactly `---`. The frontmatter ends at the next line that is exactly `---`.
The body is everything after that line; it MAY be empty.

`.yml` files (store, project, config, actors, boards) are plain YAML documents.

### 3.2 The KanArtist YAML profile

All YAML in a store MUST stay within this profile, a strict subset of YAML 1.2:

- One document per file. No anchors (`&a`), aliases (`*a`), explicit tags (`!!str`) or merge keys
  (`<<`). Mapping keys are plain scalars and MUST be unique.
- **No implicit typing.** A scalar is text; the field it belongs to decides its type. A field typed
  string accepts any scalar (`title: no` is the string "no"). A field typed boolean accepts only
  unquoted `true` or `false`; integer fields accept only unquoted decimal integers. Dates and
  timestamps are strings validated by pattern.
- An unquoted empty value, `~` or `null` is null. A null field is treated as absent.

Comments (`# …`) are allowed in `.yml` files. They are not allowed in documents' frontmatter in
canonical form (§3.3), because tools rewrite frontmatter.

### 3.3 Canonical form

Documents have exactly one canonical byte representation. Writers MUST produce it; readers MUST
accept any profile-conforming input. `ka fmt` rewrites a document to canonical form; a document
that is valid but not canonical is a `non_canonical` warning.

- Frontmatter keys appear in the order given in §4.2 (tickets) or §7.2 (comments). Absent, null and
  empty values (empty string, empty list, empty mapping) are omitted.
- Mappings and sequences use block style with two-space indentation. A sequence of mappings writes
  the first key on the `- ` line.
- Set-valued lists (`labels`, `blocked_by`, `related`) are sorted (byte order) and de-duplicated.
  Other lists keep their order. Custom fields (`fields`) are sorted by name.
- Strings are written plain when that is unambiguous, otherwise in double quotes with YAML escapes
  (`\"`, `\\`, `\n`, `\t`, `\xNN`, `\uNNNN`). A string is written plain only if it is non-empty;
  has no leading or trailing whitespace and no control characters; does not start with any of
  ``- ? : , [ ] { } # & * ! | > ' " % @ ` ``; does not contain `: ` or ` #` or end with `:`; is not
  `true`, `false`, `yes`, `no`, `on`, `off`, `y`, `n`, `null` or `~` in any case; is not a YAML 1.1
  or 1.2 number in any form (decimal, binary, octal or hexadecimal integers, with or without
  underscores; floats and exponents; sexagesimals such as `12:30`; `.inf`; `.nan`); and does not
  start like a YAML 1.1 timestamp (`dddd-d-d`, with one or two digits for month and day).
- Typed values (timestamps, dates, integers, booleans) are written unquoted.
- The frontmatter is followed by one blank line and the body with leading blank lines removed and
  trailing whitespace trimmed, then a single newline. An empty body ends the file right after the
  closing `---` line.

### 3.4 Encoding

Every text file MUST be UTF-8 without a byte-order mark and use LF line endings (`bom` and `crlf`
are errors). Stores SHOULD include a `.gitattributes` with:

```
* text=auto eol=lf
ticket.md merge=ka
```

The `merge=ka` line names `ka`'s field-wise merge driver. Git falls back to its normal text merge
on machines where the driver is not registered, so the line is harmless without `ka`.

## 4. Tickets

### 4.1 IDs

A ticket ID is `PREFIX-SUFFIX`:

- `PREFIX` is the project prefix: `^[A-Z][A-Z0-9]{0,9}$`.
- `SUFFIX` is 5 to 8 characters of Crockford base32, `0-9A-HJKMNP-TV-Z` (digits and upper-case
  letters without I, L, O, U). The default length is 5 (`id_suffix_length`, §6.6).
- Full pattern: `^[A-Z][A-Z0-9]{0,9}-[0-9A-HJKMNP-TV-Z]{5,8}$`. Example: `JK-7QK3M`.

Suffixes MUST be chosen at random (or derived from a hash, for importers). A writer MUST check that
the ID does not already exist in the project before using it. No other coordination is needed: two
writers that have not seen each other's tickets collide with probability about k²/2·32⁵ for k
concurrent creations (k = 100 gives 1.5×10⁻⁴), and a collision is detected when they sync. An ID
that has been pushed MUST NOT change; an unpushed ticket that collides takes a new ID.

Suffixes that look hand-made (fewer than three distinct characters, or a straight run through the
alphabet such as `ABCDE`) are a `low_entropy_id` warning.

To move a ticket to another project, create a new ticket there and turn the old one into a
tombstone with `moved_to:` set to the new ID.

### 4.2 Fields

Frontmatter keys, in canonical order. **R** marks required fields. Unknown keys are errors
(`unknown_key`): `statu: ready` must not silently leave a ticket without a status.

| Key | Type | Notes |
|---|---|---|
| `id` **R** | ID | Equals the directory name and uses the project prefix. |
| `title` **R** | string | One line, at most 300 characters. |
| `status` **R** | status name | A status in the project's workflow (§6.6). |
| `priority` | `P0` \| `P1` \| `P2` \| `P3` | Fixed vocabulary, P0 highest. Absent means none. |
| `kind` | kind name | One of the project's `kinds`. |
| `labels` | list of strings | Single-line, at most 64 characters, no commas. Restricted to `labels.known` when `labels.closed` is true. |
| `assignee` | actor | Who is doing the work now (may be an agent). |
| `owner` | actor | The accountable human. |
| `claim` | mapping | `by` (actor, **R**), `at` (timestamp, **R**), `session` (string, optional). Present only while claimed. |
| `parent` | ID | Parent ticket (sub-issue or epic). Children are derived. |
| `blocked_by` | list of IDs | Tickets that must finish first. `blocks` is derived. |
| `related` | list of IDs | Related tickets. Stored once, on the ticket that declares it. |
| `duplicate_of` | ID | |
| `links` | list of strings | Symbolic code links or http(s) URLs (§8.2). |
| `acceptance` | list of `{text, done}` | Acceptance criteria. `text` string (**R**), `done` boolean. |
| `milestone` | name | Reserved for milestones; lower-case name. |
| `due` | date | `YYYY-MM-DD`. |
| `fields` | mapping | Custom fields declared in config (§6.6). Values are scalars or lists of strings. |
| `moved_to` | ID | Tombstone marker. |
| `created` **R** | timestamp | |
| `created_by` | actor | |
| `updated` **R** | timestamp | Writers set it on every change. |

Value formats:

- **Timestamp:** RFC 3339 in UTC with whole seconds: `2026-11-07T13:07:22Z`.
- **Date:** `YYYY-MM-DD`.
- **Actor:** a human handle `^[a-z0-9][a-z0-9._-]{0,63}$`, an agent `agent/<name>`, or a role
  `role/<name>` (same name pattern).
- **Status, kind and milestone names:** `^[a-z][a-z0-9_-]{0,63}$`.

### 4.3 Body

The body is free Markdown describing the work. The project's `template.sections` (default:
Problem, Approach, Non-goals, Notes) are recommended headings, not required ones. Acceptance
criteria belong in the `acceptance` field, history belongs in comments, and the title is not
repeated as a heading.

Template residue is a `placeholder_text` warning: an empty checklist item (`- [ ]`), a
`{{variable}}` outside code, or a heading whose section is empty or holds only a bare `-`.

## 5. Store files

### 5.1 `.ka/store.yml`

```yaml
spec: 1
name: jk-tickets
description: Tickets for JumpKick
extends:
  - url: git@github.com:example/ka-org.git
    ref: main
```

| Key | Type | Notes |
|---|---|---|
| `spec` **R** | integer | The major spec version. This document is `1`. |
| `name` **R** | string | `^[a-z0-9][a-z0-9._-]{0,63}$`. |
| `description` | string | |
| `extends` | list | Parent stores, in order. Each item has `url` (**R**: any git URL, or a path relative to this store's root such as `../org`) and `ref` (a branch, tag or commit; default: the parent's default branch). |

### 5.2 `projects/<key>/project.yml`

```yaml
key: jk
prefix: JK
name: JumpKick
description: Build tool
repos:
  jk:
    forge: https://github.com/example/jk
    default_branch: main
config:
  kinds:
    $append:
      - spike
```

| Key | Type | Notes |
|---|---|---|
| `key` **R** | string | `^[a-z][a-z0-9-]{0,31}$`; equals the directory name. Unique in the workspace. |
| `prefix` | string | `^[A-Z][A-Z0-9]{0,9}$`; default: the key upper-cased without `-`. Unique in the workspace. |
| `name`, `description` | string | |
| `archived` | boolean | An archived project is read-only for writers. |
| `former_prefixes` | list of strings | Prefixes the project used before a rename (`ka project rename --keep-former-prefix`). A reference or lookup with a former prefix resolves to the ticket with the same suffix, so old IDs in commits and notes keep working during a transition. Each must be unused as an active prefix in the workspace. Ticket IDs and directories always use `prefix`. |
| `repos` | mapping | Code repositories for links: name → `{forge (http(s) URL), default_branch}`. |
| `config` | mapping | Project-level data config, the last layer of the chain (§6). |
| `moved_to_store` | string | Tombstone: the project now lives in another store (its name or URL). A tombstoned project holds only `project.yml`; readers skip it, so it never counts as a second home. Written by `ka project move`. |

### 5.3 `.ka/config.yml`

Data config for every project in the store. See §6.

### 5.4 `.ka/actors.yml`

```yaml
humans:
  bsant:
    name: Bryan Sant
    emails:
      - bryan@example.com
    aliases:
      - bryan.sant
agents:
  agent/claude:
    name: Claude Code
roles:
  maintainers:
    - bsant
```

Unknown actors are allowed everywhere; the registry adds display names and maps git emails and
historical spellings to handles. Actors are a union across the workspace (§6.1).

### 5.5 `boards/<name>.yml`

A saved view. The file name (`^[a-z0-9][a-z0-9_-]{0,63}\.yml$`) is the board's id.

| Key | Type | Notes |
|---|---|---|
| `name` **R** | string | Display name. |
| `description` | string | |
| `filter` | string | Query in the KanArtist filter language (defined with the engine in M1). |
| `group_by`, `swimlanes`, `sort` | string | Field names. |
| `columns`, `card_fields` | list of strings | |

Boards never store ticket membership or ordering.

## 6. Config and layering

### 6.1 Three classes of config

| Class | What | Resolved by | Why |
|---|---|---|---|
| **Data config** | Everything in `config.yml` and `project.yml` `config:` except `runtime` | The store's `extends` chain (§6.3) | Validity must not depend on who is looking. Every client computes the same config for a given ticket. |
| **Directory data** | Actors, boards | Union across the workspace | These are lookups, not rules. |
| **Runtime config** | `runtime:` (sync, fetch interval, …) and local settings | Defaults, then the store's recommendations, then the user's settings, environment and flags; the user wins | It changes behavior, never validity. |

### 6.2 Built-in defaults

The bottom layer of every chain is [`defaults.yml`](defaults.yml): the workflow `triage, backlog,
ready, in_progress, blocked, review, done, canceled`, the kinds `feature, bug, chore, research,
design, refactor, perf`, open labels, no custom fields, a 24-hour stale-claim threshold and size
limits.

### 6.3 The chain and merge rules

For a ticket in project `P` whose home store is `S`, the effective data config is built by applying
these layers in order, **later layers overriding earlier ones**:

1. built-in defaults;
2. the config chain of each parent in `S`'s `extends`, left to right (each parent's own parents
   first, depth-first);
3. `S/.ka/config.yml`;
4. the `config:` block of `S/projects/P/project.yml`.

A store reached more than once (a diamond) is applied once, at its first position. A cycle is a
`config_cycle` error. Chains deeper than 4 are an error.

Merging one layer onto the result so far:

| Layer value | Rule |
|---|---|
| A mapping, where the existing value is a mapping | Merged key by key, recursively. |
| `null` | Deletes the key. |
| `{$append: [...]}` and/or `{$remove: [...]}` | Patches a list of strings: remove first, then append the items not already present. |
| Anything else (scalars, lists) | Replaces the existing value. Lists such as `workflow.statuses` and `kinds` are replaced as a whole. |

### 6.4 Locks

```yaml
locked:
  - workflow
  - fields.security_review
```

A layer's `locked` paths apply to every **later** layer. A later layer that sets, deletes, patches
or replaces a locked path (or a mapping containing one) gets a `config_locked` error, and its value
is ignored. Locks accumulate down the chain; a layer can remove only its own locks. Without a lock,
a child may change a key in either direction (tighten or loosen).

### 6.5 Unavailable parents

A writer MUST NOT write tickets to a store whose chain it cannot resolve, unless it has a cached
copy of the missing parent's config. Checkers report an unresolvable parent as
`config_parent_unavailable`. Parents SHOULD be readable by everyone who can read the store.

### 6.6 Keys

| Key | Type | Default | Notes |
|---|---|---|---|
| `id_suffix_length` | integer 5–8 | 5 | Length of new ID suffixes. Existing IDs keep theirs. |
| `workflow.statuses` | list of `{name, category, blocked?, description?}` | §6.2 | Status names are free; categories are fixed: `triage`, `backlog`, `unstarted`, `started`, `completed`, `canceled`. |
| `workflow.initial` | status | `backlog` | Status of new tickets. |
| `workflow.agent_created_initial` | status | (none) | Status of tickets agents create, e.g. `triage`. |
| `kinds` | list of names | §6.2 | |
| `labels.closed` | boolean | `false` | When true, labels must be in `labels.known`. |
| `labels.known` | list of strings | (none) | |
| `fields` | mapping | `{}` | Custom fields: name (`^[a-z][a-z0-9_]{0,63}$`) → `{type, values?, description?}`. Types: `string` (one line), `text`, `number`, `integer`, `boolean`, `enum` (needs `values`), `date`, `timestamp`, `actor`, `url`, `id`, `list` (of strings). Undeclared fields on tickets are errors. |
| `claims.stale_after` | duration | `24h` | Durations are Go-style (`90s`, `15m`, `24h`) or whole days (`7d`). |
| `comments.max_bytes` | integer | 65536 | |
| `comments.progress_min_interval` | duration | `2m` | Minimum gap between progress comments on a ticket (enforced by writers). |
| `attachments.max_bytes` | integer | 5242880 | Per file under `files/`. |
| `template.sections` | list of strings | §6.2 | Recommended body headings. |
| `locked` | list of dotted paths | `[]` | §6.4. |
| `runtime` | mapping | §6.2 | Runtime recommendations (§6.1); not validated by this spec. |

## 7. Comments

### 7.1 Files

Comments live in `tickets/<ID>/comments/`, one file each, named
`<yyyymmddThhmmssZ>-<XXXX>.md`: the creation time in UTC and four random Crockford base32
characters (pattern `^\d{8}T\d{6}Z-[0-9A-HJKMNP-TV-Z]{4}\.md$`). Names sort in time order and
never collide. The timestamp SHOULD equal `created` (`comment_time_mismatch`).

**Comments are immutable.** To edit one, add a comment with `supersedes:`; to withdraw one, add a
comment with `retracts:`. Removing a file is reserved for emergencies such as a leaked secret.

### 7.2 Fields

| Key | Type | Notes |
|---|---|---|
| `type` **R** | enum | `note`, `progress`, `question`, `answer`, `decision`, `resolution`, `approval`, `system`. |
| `author` **R** | actor | |
| `on_behalf_of` | human actor | The human an agent works for. |
| `session` | string | The agent session. |
| `created` **R** | timestamp | |
| `to` | actor | Addressee of a question or approval request. |
| `answers` | comment filename | **R** on `answer` and `approval`; only allowed there. |
| `choice` | integer ≥ 0 | Index into the question's `options`; only on `answer`. |
| `approved` | boolean | **R** on `approval`; only allowed there. |
| `resolution` | enum | `done`, `canceled`, `duplicate`, `wontfix`. **R** on `resolution`; only allowed there. |
| `supersedes`, `retracts` | comment filename | |
| `options` | list of strings | Choices offered by a `question`; only allowed there. |

`answers`, `supersedes` and `retracts` MUST name a comment of the same ticket (`answer_dangling`).
A question is open until some `answer` names it; state is derived, never edited in.

## 8. Relations and links

### 8.1 Relations

`parent`, `blocked_by`, `related`, `duplicate_of` and `moved_to` hold ticket IDs, possibly in
other projects and other stores. Each fact is stored once, on the ticket that declares it.

- A reference to a project in the workspace MUST name an existing ticket (`dangling_ref`).
- A reference whose prefix belongs to no project in the workspace is external (`external_ref`,
  informational).
- A ticket MUST NOT reference itself (`self_ref`). The `parent` and `blocked_by` graphs MUST be
  acyclic (`cycle`).

### 8.2 Links

A link is an http(s) URL or a **symbolic code link**:

```
<repo>://<path>[@<ref>][#L<line>[-<line>]]
jk://server/resolver/PubGrub.java@3f2a9c1#L42
```

`<repo>` names an entry in some project's `repos` (`unknown_repo` otherwise). Use a commit SHA as
`ref` for durable evidence and a branch name for "the current thing". Clients resolve a link to a
local checkout when they know one, else to `<forge>/blob/<ref>/<path>#L<line>`.

## 9. Validation

### 9.1 Severities

**Errors** make data invalid; writers MUST NOT produce them and CI SHOULD reject them. **Warnings**
are defects that do not break readers; strict mode (`ka check --strict`) treats them as errors.
**Info** findings are observations.

### 9.2 Diagnostic codes

These codes are stable. Tools SHOULD use them so that findings are comparable across
implementations.

| Code | Severity | Meaning |
|---|---|---|
| `crlf` | error | A file contains CR characters. |
| `bom` | error | A file starts with a UTF-8 byte-order mark. |
| `frontmatter_missing` | error | A document has no (or an unclosed) frontmatter block. |
| `yaml_syntax` | error | YAML does not parse, or is not a mapping where one is required. |
| `yaml_feature` | error | YAML uses a feature outside the profile (§3.2). |
| `duplicate_key` | error | A YAML mapping repeats a key. |
| `non_canonical` | warning | A valid document is not in canonical form (§3.3). |
| `unknown_key` | error | A key not defined for this file. |
| `missing_field` | error | A required field is absent or empty. |
| `invalid_value` | error | A value has the wrong type or format. |
| `unknown_status` | error | Status not in the project's workflow. |
| `unknown_kind` | error | Kind not in the project's kinds. |
| `unknown_label` | error | Label outside a closed label set. |
| `unknown_field` | error | Custom field not declared in config. |
| `invalid_field_value` | error | Custom field value does not match its type. |
| `placeholder_text` | warning | Template residue in a body (§4.3). |
| `low_entropy_id` | warning | ID suffix does not look random (§4.1). |
| `unknown_repo` | warning | A symbolic link names an undeclared repo. |
| `invalid_store` | error | `.ka/store.yml` is missing. |
| `spec_unsupported` | error | The store uses a spec version the tool does not support. |
| `project_key_mismatch` | error | `project.yml` `key` differs from the directory name. |
| `project_missing_file` | error | A project directory has no `project.yml`. |
| `ticket_missing_file` | error | A ticket directory has no `ticket.md`. |
| `id_dir_mismatch` | error | A ticket's `id` differs from its directory name. |
| `id_prefix_mismatch` | error | A ticket's ID does not use its project's prefix. |
| `duplicate_project` | error | Two stores in a workspace define the same project key. |
| `moved_project_has_tickets` | warning | A project tombstone (`moved_to_store`) still has tickets; they are ignored. |
| `duplicate_prefix` | error | Two projects use the same prefix. |
| `duplicate_id` | error | Two tickets share an ID. |
| `stray_file` | warning | An unexpected file or directory (§2). |
| `comment_filename` | error | A comment file name does not follow §7.1. |
| `comment_time_mismatch` | warning | A comment's `created` differs from its file name. |
| `dangling_ref` | error | A relation names a missing ticket in a known project. |
| `external_ref` | info | A relation names a project outside the workspace. |
| `self_ref` | error | A ticket references itself. |
| `cycle` | error | A cycle in `parent` or `blocked_by`. |
| `answer_dangling` | error | `answers`/`supersedes`/`retracts` names a missing comment. |
| `stale_claim` | info | A claim is older than `claims.stale_after`. |
| `archived_project` | info | A ticket in an archived project is still in a started status. |
| `secret_suspect` | warning | Text that looks like a credential. |
| `comment_too_large` | warning | A comment exceeds `comments.max_bytes`. |
| `file_too_large` | warning | An attachment exceeds `attachments.max_bytes`. |
| `config_invalid` | error | Config does not validate. |
| `config_locked` | error | A layer overrides a locked path (§6.4). |
| `config_cycle` | error | `extends` forms a cycle. |
| `config_parent_unavailable` | error | A parent store cannot be resolved. |

## 10. Commits

This section is informative in version 1; `ka` follows it and the activity feed reads it.

- One logical change per commit, on a linear history (rebase, never merge).
- The subject reads like an activity feed: `JK-7QK3M: in_progress → review`.
- Structured trailers describe the change: `Ka-Op` (the operation, replayable during rebase),
  `Ka-Actor`, `Ka-On-Behalf-Of`, `Ka-Session`, `Ka-Spec`. Trailers are hints: readers derive what
  changed from the diff and use trailers only to enrich it.
- `Ka-Op` is `<op> <json>`: the operation name (`create`, `update`, `claim`, `release`,
  `transition`, `comment`, `check`) followed by a single-line JSON object holding everything needed
  to apply it again, including its timestamp (`at`) and any generated comment filename. A tool
  that replays an operation re-checks its preconditions against the new state first.

## 11. Writing by hand

An agent or human without `ka` can produce valid data:

1. **New ticket:** pick 5 random characters from `0123456789ABCDEFGHJKMNPQRSTVWXYZ`, form
   `<PREFIX>-<SUFFIX>`, and confirm `projects/<key>/tickets/<ID>/` does not exist. Write
   `ticket.md` with at least `id`, `title`, `status`, `created` and `updated`.
2. **Comment:** add a new file under `comments/` named with the current UTC time and 4 random
   characters. Never edit an existing comment.
3. **Claim:** set `claim` (`by`, `at`) and `assignee`, commit, and push before starting work. If
   the push is rejected, `git pull --rebase` and check whether someone else claimed it first.
4. **Always** `git pull --rebase`; never create merge commits.
5. Run `ka check` (or let CI run it), and `ka fmt` to canonicalize.

Minimal ticket:

```markdown
---
id: JK-7QK3M
title: Fix the flaky cache test
status: ready
created: 2026-11-07T13:07:22Z
updated: 2026-11-07T13:07:22Z
---

## Problem

`CacheTest.evicts` fails about one run in twenty on Windows.
```

## 12. Versioning

The spec version is the integer `spec` in `.ka/store.yml`. Additions that existing readers can
ignore (new optional keys in new files, new diagnostic codes, new reserved directories) do not
change it. A change that would make existing valid data invalid, or valid data mean something
different, increments it, and `ka migrate` upgrades a store in one commit. A tool MUST NOT write to
a store whose spec version it does not support.
