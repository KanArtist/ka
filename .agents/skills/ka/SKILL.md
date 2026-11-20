---
name: ka
description: Track work as KanArtist tickets with ka. Use when picking up, claiming, updating, commenting on or closing tickets, asking a human a question, or when the user mentions tickets, the board, KanArtist or ka. Also use when the user asks to compact a store, reduce or shrink its git history, flatten or squash ticket history, or says the ticket repository is too big.
---

# KanArtist tickets (ka)

KanArtist is a work tracker for agents and the people who direct them.
Tickets are plain files in git repositories called stores; `ka` reads and
writes them, commits and pushes. You never edit ticket files by hand while
`ka` works, and you never need to `git pull` or `git push` a store yourself.

Use the `ka` MCP tools when they are loaded (`ka_next`, `ka_get`, ...);
otherwise run the same operations with the `ka` CLI. `ka <command> -h`
explains any command.

## The loop

1. **Find work.** `ka_next` (CLI: `ka next`, then `ka claim <ID>`) picks the
   best ready ticket, unblocked and unclaimed, and claims it for you. To work
   on a specific ticket, `ka_claim` it. If the claim is lost, someone else
   holds it: pick another ticket, never take it over.
2. **Read it.** `ka_get` (`ka show <ID>`) gives the description, acceptance
   items, relations and a digest of comments: open questions, decisions,
   recent progress. Read the decisions before you start.
3. **Work, and say what you're doing.** Post progress as you reach milestones:
   `ka_comment` with type `progress` (`ka comment <ID> --type progress -m "…"`).
   Record choices that others must respect as type `decision`. Tick acceptance
   items as they pass: `ka_check` (`ka check-item <ID> <index>`).
4. **Blocked on a person?** `ka_ask` (`ka ask <ID> -m "question"
   [--option A --option B]`) asks a human and shows up in their inbox. Don't
   wait idle: move to other work, and check `ka_inbox` (`ka inbox`) later.
5. **Finish.** `ka_transition` to the done status with a summary of what
   changed and where (`ka move <ID> done -m "…"`). The summary is recorded as
   the resolution and unblocks dependent tickets. If you stop without
   finishing, `ka_release` (`ka release <ID>`) so others can pick it up.

## Referring to tickets

`ka` shows each ticket as its **reference**: the ID plus a short slug, such
as `JK-7QK3M-compiler-warnings`. Use the reference when you mention a ticket
in messages, commits, PR descriptions and notes, so readers know what it is
without looking it up. Any command accepts either the reference or the bare
ID; the slug part is ignored when looking a ticket up, so an old reference
still works after the slug changes.

## Creating and changing tickets

- `ka_schema` (`ka schema`) lists projects, statuses, kinds, labels and custom
  fields. Use those names exactly; don't invent statuses.
- `ka_create` (`ka new --project <key> -t "Title" --slug short-name --kind feature -m - < body.md`)
  creates tickets; follow-ups found while working belong in new tickets, not
  in scope creep. Give each a `slug`: 2-4 lower-case words that name the work
  (at most 24 characters), such as `compiler-warnings`. Without one, `ka`
  derives it from the title.
- `ka_update` (`ka edit <ID> --set priority=P1 --add labels=ci`; no flags opens
  an editor) changes fields, labels, blockers and the description. Status
  changes always go through `ka_transition`.
- `ka_search` (`ka ls <query>`) uses the filter language:
  `project:jk status:in_progress assignee:me label:ci "free text"`.

## Initiatives and epics

Work can be grouped: project > initiative > epic > ticket. An initiative
(kind `initiative`, no parent) is a long-running theme such as "GDPR"; an epic
(kind `epic`, parent an initiative or none) is a body of work; tickets point
at either with `parent`. `ka_next` never hands out an initiative or epic.

To run one ("implement the GDPR initiative"):

1. `ka_tree` (`ka tree <ID>`) shows the initiative, its epics and tickets with
   status, claims and a progress rollup. `ka_tree` without an ID lists them.
2. Pick an epic; each worker calls `ka_next` with `within: <epic>`
   (`ka next --within <ID> --claim`) and works the loop above.
3. When an epic's rollup is all done, close it with `ka_transition` and a
   summary; close the initiative last. Nothing closes them automatically.

Plan new ones with the `ka/plan-initiative` and `ka/plan-epic` prompts;
`ka_search` scopes with `initiative:<ID>` or `epic:<ID>`.

## Identity

Your actor name is `agent/<you>`, set by `KA_ACTOR` (the MCP server is
registered with it). Comments, claims and commits record it, so people can
see which agent did what.

## Compacting history

A store's git history can be squashed into one commit to shrink the
repository (`ka_compact`, CLI `ka compact --store NAME`). Only do this when
the user asks for it (compaction, history reduction, a repository that is too
big); never on your own initiative.

1. Tell the user what it does: it rewrites the store's shared history (a force
   push) and cannot be undone on the remote. Tickets and their current state
   stay; the commit-by-commit history goes, except for a summary file kept in
   `history/`. Other ka clients adopt the new history by themselves; plain git
   clones and forks must re-clone. Suggest a backup first
   (`git clone --mirror`).
2. Run the dry run: `ka_compact` without `confirm` (`ka compact --store NAME
   --dry-run`) and show the user what it reports.
3. Offer to write the narrative for the summary: what the dropped history held
   (major work, decisions, who did what), drafted from the activity feed
   (`ka watch --history 500`, or `GET /activity`) and the tickets. Show it to the
   user.
4. Only after the user explicitly says to go ahead, run it: `ka_compact` with
   `confirm` set to the store name and `summary` (`ka compact --store NAME
   --summary-file notes.md --yes`). Report the result, including warnings.

`ka_history` (`ka history`) lists earlier compactions and shows a summary.

## When something is off

`ka sync` (`ka_sync`) reports each store's sync state. `ka doctor` checks
git, clones and the merge driver; `ka doctor --fix` repairs them. Report
errors to the user with the exact message rather than working around them.
