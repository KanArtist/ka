---
name: ka
description: Track work as KanArtist tickets with ka. Use when picking up, claiming, updating, commenting on or closing tickets, asking a human a question, or when the user mentions tickets, the board, KanArtist or ka.
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

## Creating and changing tickets

- `ka_schema` (`ka schema`) lists projects, statuses, kinds, labels and custom
  fields. Use those names exactly; don't invent statuses.
- `ka_create` (`ka new --project <key> -t "Title" --kind feature -m - < body.md`)
  creates tickets; follow-ups found while working belong in new tickets, not
  in scope creep.
- `ka_update` (`ka edit <ID> --set priority=P1 --add labels=ci`; no flags opens
  an editor) changes fields, labels, blockers and the description. Status
  changes always go through `ka_transition`.
- `ka_search` (`ka ls <query>`) uses the filter language:
  `project:jk status:in_progress assignee:me label:ci "free text"`.

## Identity

Your actor name is `agent/<you>`, set by `KA_ACTOR` (the MCP server is
registered with it). Comments, claims and commits record it, so people can
see which agent did what.

## When something is off

`ka sync` (`ka_sync`) reports each store's sync state. `ka doctor` checks
git, clones and the merge driver; `ka doctor --fix` repairs them. Report
errors to the user with the exact message rather than working around them.
