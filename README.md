# KanArtist (`ka`)

A work tracker for teams whose work is mostly done by AI agents, with git as the database.
Agents are the primary users; humans get a live window into what they are doing.

**New here? Start with [Getting started](docs/getting-started.md).** All documentation:
[docs/](docs/README.md).

- **For agents:** [docs/agent-guide.md](docs/agent-guide.md)
- **Running a team daemon:** [docs/operations.md](docs/operations.md)
- **The Store Spec (the on-disk contract):** [spec/v1/README.md](spec/v1/README.md)
- **Product requirements:** [docs/prd.md](docs/prd.md)

## Status: M1 and M2 done; M3 and M4 built, awaiting review; 1.0 not yet released

| Piece | Where |
|---|---|
| Store Spec v1: prose, JSON Schemas, built-in defaults, conformance corpus | [`spec/v1/`](spec/v1/) |
| `kaspec`: parse, canonical format, config layering with locks, validation | [`pkg/kaspec/`](pkg/kaspec/) |
| Engine: workspaces, index, queries, operations, sync with operation replay, merge driver | [`internal/engine/`](internal/engine/) |
| `ka mcp`: 15 MCP tools with workspace-specific schemas | [`internal/mcpserver/`](internal/mcpserver/) |
| The `ka` CLI, with prompts, completion, `ka watch`, hooks | [`internal/cli/`](internal/cli/) |
| `ka daemon`: REST, OpenAPI 3.1, SSE, MCP over HTTP, presence, remote mode (tokens, proxies), webhooks, metrics | [`internal/api/`](internal/api/) |
| Web UI (Angular, embedded): Activity, Board (drag and drop, saved boards), Table, ticket editing, Agents, Inbox, Docs, Sync & health, Settings | [`web/`](web/) |
| PRD scenarios S1–S9, the multi-clone simulator, token budgets | [`internal/scenario/`](internal/scenario/) |
| `ka check --ci` GitHub Action | [`actions/check/`](actions/check/) |
| Releases (GoReleaser, container image) — not yet published | [`.goreleaser.yaml`](.goreleaser.yaml), [`Dockerfile`](Dockerfile) |
| One-time v0 importer | [`tools/migrate-v0/`](tools/migrate-v0/) |

## Install

Requires git 2.40+ at run time. Until the first release, build it with Go 1.27+:

```bash
go install github.com/JumpKickOSS/ka/cmd/ka@latest
source <(ka completion bash)          # optional; also zsh, fish, powershell
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
ka open [ID]                         # the web UI (starts the daemon if needed)
ka watch [QUERY]                     # the live activity stream in the terminal
ka config explain [KEY]              # effective config and the file that set each value
ka project move KEY --to STORE       # re-home a project; IDs stay the same
ka hooks install                     # pre-commit: ka fmt --staged && ka check --staged
ka daemon / ka daemon status / ka daemon stop   # REST API on 127.0.0.1:7711
ka token create --actor NAME         # API tokens for a remote daemon (docs/operations.md)
```

Every read command takes `--json`. `ka help` lists everything.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md).

```bash
go test ./...                                          # everything (the S5 simulator takes ~30 s)
go test -short -race ./...                             # what CI runs on all three OSes
KA_V0_CORPUS=~/src/oss/kanartist go test ./tools/...   # plus the real v0 corpus, if you have it
```

## License

[Apache-2.0](LICENSE)
