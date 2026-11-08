# KanArtist (`ka`)

A work tracker for teams whose work is mostly done by AI agents, with git as the database.
Agents are the primary users; humans get a live window into what they are doing.

- **Product requirements:** [docs/prd.md](docs/prd.md)
- **The Store Spec (the on-disk contract):** [spec/v1/README.md](spec/v1/README.md)
- **How the engine, CLI and MCP server work:** [docs/engine.md](docs/engine.md)
- **The daemon and REST API:** [docs/api.md](docs/api.md)
- **Migrating from KanArtist v0:** [docs/migration.md](docs/migration.md)

## Status: M1 done; M2 (daemon and live UI) in progress

| Piece | Where |
|---|---|
| Store Spec v1: prose, JSON Schemas, built-in defaults, conformance corpus | [`spec/v1/`](spec/v1/) |
| `kaspec`: parse, canonical format, config layering, validation | [`pkg/kaspec/`](pkg/kaspec/) |
| Engine: workspaces, index, queries, operations, sync with operation replay, merge driver | [`internal/engine/`](internal/engine/) |
| `ka mcp`: 15 MCP tools with workspace-specific schemas | [`internal/mcpserver/`](internal/mcpserver/) |
| The `ka` CLI | [`internal/cli/`](internal/cli/) |
| `ka daemon`: REST `/api/v1`, OpenAPI 3.1, SSE events, MCP over HTTP, presence, local token auth (M2) | [`internal/api/`](internal/api/) |
| PRD scenarios, the multi-clone simulator, token budgets | [`internal/scenario/`](internal/scenario/) |
| One-time v0 importer (repeatable until cut-over) | [`tools/migrate-v0/`](tools/migrate-v0/) |

The web UI is the rest of M2 ([PRD §21](docs/prd.md#21-milestones)).

## Install

Requires Go 1.27+ to build, and git 2.40+ at run time.

```bash
go install github.com/JumpKickOSS/ka/cmd/ka@latest
```

## For agents (MCP)

```bash
claude mcp add ka -- ka mcp       # Claude Code; other clients: command "ka", args ["mcp"]
```

Start the agent inside a store, or in a code repo whose root has a `.ka.yml` pointer:

```yaml
store: git@github.com:example/tickets.git   # or a path to a local store checkout
project: jk
```

The loop is `ka_next` (find and claim) → work → `ka_comment` / `ka_check` →
`ka_transition status=done summary=…`. Follow-ups go in one `ka_create` call; questions for a
human go through `ka_ask`.

## For humans (CLI)

```bash
ka init --project jk                 # new store in the current git repo
ka ls is:ready project:jk            # query: project:jk kind:bug -label:infra is:blocked words
ka next [--claim]                    # the best ready ticket
ka show JK-7QK3M [--comments]
ka new -t "Fix the flaky cache test" --kind bug --priority P1 --accept "passes 100 runs"
ka edit JK-7QK3M --set priority=P0 --add labels=cache   # no flags: $EDITOR
ka move JK-7QK3M done -m "Fixed the race in the eviction thread."
ka inbox / ka answer JK-7QK3M --choice 1 / ka approve JK-7QK3M
ka sync / ka check / ka fix / ka doctor [--fix]
ka daemon / ka daemon status / ka daemon stop   # REST API on 127.0.0.1:7711
```

Every read command takes `--json`. `ka help` lists everything.

## Development

```bash
go test ./...                                          # everything (the S5 simulator takes ~30 s)
go test -short -race ./...                             # what CI runs on all three OSes
KA_V0_CORPUS=~/src/oss/kanartist go test ./tools/...   # plus the real v0 corpus, if you have it
```

Spec changes and code changes travel together: tests fail if the diagnostic-code table in the spec,
the built-in defaults, the JSON Schemas and the Go validator disagree.

## License

[Apache-2.0](LICENSE)
