# Migrating from KanArtist v0

`tools/migrate-v0` imports the v0 repository (Python era, `projects/<key>/tickets/*.md`) into
Store Spec v1 stores. It is a one-time tool, kept out of the `ka` binary, but it is built to be
**run as many times as you like** until cut-over, so work that is still in flight in v0 keeps
flowing into v1 as it lands.

## Running it

```bash
go run ./tools/migrate-v0 --v0 ~/src/oss/kanartist --out ~/src/oss/ka-stores
```

| Flag | Meaning |
|---|---|
| `--v0` | The v0 checkout (read-only; never modified). |
| `--out` | Directory holding one subdirectory per v1 store. |
| `--layout split` | One store per project plus `org` (default, decision D5). `gyggly-java` shares the `gyggly` store. |
| `--layout single` | Everything in the `org` store. Ticket IDs are identical in both layouts. |
| `--org-url` | The org store URL written into project stores' `extends` (default `../org`; set the real git URL once the repos exist). |
| `--commit` | Commit in each store directory that is a git repository. Never pushes. |
| `--dry-run` | Report what would change; write nothing. |
| `--force` | Overwrite files modified since the last run (see below). |

Each run prints a summary, writes `<out>/migrate-v0-report.md` (counts before and after,
every normalization, dropped values, `ka check` findings), and runs `ka check` over the result. It
exits non-zero if the output has errors or there are conflicts.

## What makes it repeatable

- **Stable IDs.** A v0 ticket's identity is its file name (v0 never renamed tickets). Its v1 ID is
  derived from a hash of project and file name. On every run the migrator first re-finds tickets
  from earlier runs (by ID and `fields.legacy_id`), then assigns new tickets the first free
  candidate, so new tickets can never take an ID that an earlier ticket already has, and collisions
  resolve the same way every time.
- **Deterministic output.** Same input, same bytes. A re-run with no v0 changes reports
  `0 created, 0 updated, 0 deleted`.
- **Ownership tracking.** `<out>/.migrate-v0-state.json` records the hash of every file the
  migrator wrote. On a re-run:
  - a file that still has the content the migrator last wrote is updated if v0 changed;
  - a file that was **edited since** (for example, a v1 ticket you started using) is left alone and
    reported as a conflict, until you either revert the edit or pass `--force`;
  - files the migrator wrote earlier but v0 no longer produces (a deleted v0 ticket) are removed,
    unless edited;
  - files the migrator never wrote (tickets created natively in v1) are never touched.
- **References follow.** `depends_on` and `JK-1234` mentions in titles, bodies, comments and
  fields are rewritten to v1 IDs on every run, so a ticket filed in v0 tomorrow that mentions an
  old ticket links correctly. A legacy ID that maps to two tickets (v0 has one duplicate,
  `JK-4663`) is left as written.

## What it does to each ticket

| v0 | v1 |
|---|---|
| `JK-4661` | A new ID such as `JK-3B0SH`; the old ID in `fields.legacy_id` |
| `depends_on` | `blocked_by` (dangling targets dropped and listed in the report) |
| `kind: go-do` | `kind: feature` (other invalid kinds also become `feature`) |
| `bsant`, `bryan.sant`, `Bryan Sant` / `claude` / `unknown` | `bsant` / `agent/claude` / no assignee |
| Dates (`2026-09-01`) | Timestamps at midnight UTC |
| `claimed_at` on an `in_progress` ticket | `claim: {by, at}` (claims on other statuses are dropped) |
| `## Acceptance` checklist | The `acceptance` field (left in the body if it holds anything but a checklist) |
| `## Outcome`, `## Resolution`, `## Done`, `## Landed`, `## Shipped`, `## Closing note`, `## Closed by audit (…)`, … | Comments: `resolution` on done tickets, `note` otherwise, dated from the heading |
| `## Audit note`, `## Amendment`, `## Correction`, `## Progress` | `note` / `progress` comments |
| `status_note` | A `note` comment |
| `branch`, `source`, `estimate`, `order`, `shipped`, `deliverable`, `anti_goal` | Declared custom fields (empty values dropped) |
| Empty template sections, the repeated `# JK-1234 — Title` heading | Removed |
| Titles over 300 characters (9 tickets) | Shortened; the full title opens the body |
| CRLF line endings (112 tickets) | LF |
| `projects/gyggly/archive/2026/*` | Project `gyggly-java` (archived, prefix `GYJ`) |
| `projects/<key>/docs/**` | Copied unchanged |

The rules specific to this corpus (assignee aliases, archive routing, kind renames) are tables at
the top of `tools/migrate-v0/v0.go`.

## Cut-over

1. Create the store repositories (`org`, `jk`, `gyggly`, `jcrm`, `knest`, `nerfherdr`), clone
   them under one directory, and run the migrator into it with `--org-url <org repo URL> --commit`.
   Push.
2. Keep working in v0. Re-run (`--commit`, push) whenever you want v1 to catch up.
3. When ready: stop writing to v0, run a final migration, push, then archive the v0 repository
   read-only and point its README at the new stores.
4. Delete `.migrate-v0-state.json`; from here on the stores are maintained by `ka` and agents.
