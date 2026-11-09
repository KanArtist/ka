<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.svg">
    <img src="docs/assets/logo-light.svg" alt="KanArtist logo: a figure in a fedora pinning cards to a board" width="160">
  </picture>
</p>

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

The easy way: clone this repository and ask your coding agent to set KanArtist up.

```bash
git clone https://github.com/KanArtist/ka && cd ka
claude "set up KanArtist"         # or codex, cursor-agent, agy, copilot, grok, muse…
```

[AGENTS.md](AGENTS.md) tells the agent what to do: pin the checkout to the latest release, run
the install script, run `ka setup` (which registers the ka MCP server and skill with every coding
agent on the machine and serves the web UI from the checkout's `ui/`), and connect your ticket
stores. To update, check out a newer tag and ask again.

By hand (requires git 2.40+ at run time):

```bash
curl -fsSL https://raw.githubusercontent.com/KanArtist/ka/main/install.sh | sh   # Linux, macOS
irm https://raw.githubusercontent.com/KanArtist/ka/main/install.ps1 | iex         # Windows (PowerShell)
brew install kanartist/tap/ka                    # macOS, Linux
scoop bucket add kanartist https://github.com/KanArtist/scoop-bucket
scoop install kanartist/ka                       # Windows
go install github.com/KanArtist/ka/cmd/ka@latest   # anywhere, with Go 1.27+
docker run --rm ghcr.io/kanartist/ka version     # the container image (a team daemon)
```

The script checks the download against the release's signed checksums (and the signature
itself when `cosign` is installed), installs to `~/.local/bin` (`/usr/local/bin` as root) and
upgrades when run again; `KA_VERSION` pins a release and `KA_INSTALL_DIR` picks the directory.
With Go 1.27+ installed, the scripts build from source when there is no release for your platform
or version, with `KA_FROM_SOURCE=1`, or with `KA_VERSION=main` for the newest commit. Run from a
checkout of this repository, they install the release matching the checked-out tag, or build the
checkout when it isn't on a tag. Then run `ka setup`. Read them first if you prefer:
[install.sh](install.sh), [install.ps1](install.ps1).

Debian, Ubuntu, Fedora and RHEL can install the package (`amd64` or `arm64`):

```bash
curl -fsSLO https://github.com/KanArtist/ka/releases/latest/download/ka_linux_amd64.deb && sudo apt install ./ka_linux_amd64.deb
sudo dnf install https://github.com/KanArtist/ka/releases/latest/download/ka_linux_amd64.rpm
```

Or download an archive from [the releases](https://github.com/KanArtist/ka/releases/latest).

```bash
source <(ka completion bash)          # optional; also zsh, fish, powershell
```

## For agents (MCP)

```bash
ka setup              # registers the MCP server and skill with every coding agent found
ka setup --dry-run    # shows what it would change; --agents claude,codex limits it
```

Or by hand: `claude mcp add -s user ka -- ka mcp` (other clients: command `ka`, args `["mcp"]`).
`ka skill` prints the guide agents follow.

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
ka project rename KEY --key NEW --prefix P   # new key and ID prefix; references follow
ka store set-url NAME URL            # a store's repository moved; keeps the clone
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
