# Getting started

KanArtist (`ka`) keeps tickets as files in a git repository, a **store**. Agents work tickets
through MCP; people watch and steer them in a web UI or the CLI. Everything is a commit, so the
history is in `git log` and every clone can work offline.

This guide takes about ten minutes: install, create a store, connect an agent, open the UI, and
share the store with a teammate.

## 1. Install

You need git 2.40 or later. The quickest route is to let a coding agent do it: clone
`https://github.com/KanArtist/ka`, start your agent in the clone and ask it to set KanArtist up
([AGENTS.md](../AGENTS.md) has the steps). By hand:

```bash
curl -fsSL https://raw.githubusercontent.com/KanArtist/ka/main/install.sh | sh   # Linux, macOS
irm https://raw.githubusercontent.com/KanArtist/ka/main/install.ps1 | iex         # Windows
brew install kanartist/tap/ka                    # macOS, Linux
scoop bucket add kanartist https://github.com/KanArtist/scoop-bucket
scoop install kanartist/ka                       # Windows
go install github.com/KanArtist/ka/cmd/ka@latest   # anywhere, with Go 1.27+
docker run --rm ghcr.io/kanartist/ka version     # the container image (a team daemon)
sudo apt install ./ka_linux_amd64.deb            # after downloading releases/latest/download/ka_linux_amd64.deb
sudo dnf install https://github.com/KanArtist/ka/releases/latest/download/ka_linux_amd64.rpm

ka version
```

## 2. Create a store

A store can be its own repository (recommended for a team) or live inside a code repository.

```bash
mkdir tickets && cd tickets
git init -b main
ka init --project app            # .ka/store.yml, a project "app" (IDs APP-XXXXX), AGENTS.md
git add -A && git commit -m "Create the ticket store"
```

To share it, push it to any git host your team can reach:

```bash
git remote add origin git@github.com:example/tickets.git
git push -u origin main
```

From now on, every `ka` write commits **and pushes** (claims always push first, so two agents
never claim the same ticket).

## 3. File and find work

```bash
ka new -t "Add a health check endpoint" --kind feature --priority P1 --status ready \
  --accept "GET /healthz returns 200" --accept "documented in the README"
ka ls                              # open tickets
ka ls is:ready kind:feature        # queries: project:app label:x -label:y assignee:agent/* words
ka show APP-7QK3M                  # use an ID from the list
```

New tickets start in `backlog` unless you say otherwise; agents only pick up `ready` work, so
promote a ticket when it is ready to be worked: `ka move APP-7QK3M ready`. Run `ka new` with no
arguments in a terminal and it asks for the title, kind and priority.

## 4. Connect an agent

```bash
ka setup        # registers the ka MCP server and skill with every coding agent on this machine
# By hand: claude mcp add -s user ka -- ka mcp (other clients: command "ka", arguments ["mcp"]).
```

Then ask the agent to "take the next ticket". The loop is:

1. `ka_next` finds the highest-priority ready ticket and claims it.
2. The agent works, posting `ka_comment` progress notes and ticking acceptance items with `ka_check`.
3. `ka_transition status=done summary=…` closes it with a resolution comment.
4. Follow-ups go in one `ka_create` call; questions for you go through `ka_ask`.

Agents without MCP can use the same CLI commands. Agents that only edit files follow the
protocol in `AGENTS.md` (written by `ka init`), and `ka check` catches mistakes.

**Working from a code repository.** Put a `.ka.yml` at the code repository's root and agents
started there find the store with no setup:

```yaml
store: git@github.com:example/tickets.git   # or a path to a local checkout of the store
project: app
```

## 5. Watch and steer

```bash
ka open                    # the web UI; starts a local daemon on 127.0.0.1:7711
ka watch                   # or the same activity stream in the terminal
```

In the UI:

| View | What it is for |
|---|---|
| **Activity** | What agents and people are doing, live, grouped by who did what to which ticket. |
| **Board** | Columns by status. Drag cards, or move them with Shift+arrows. Save filtered views as boards. |
| **Table** | Every ticket as rows; pick columns (including custom fields), group, sort, edit inline. |
| **Agents** | Connected agent sessions, their claims and last actions; release stale claims. |
| **Inbox** | Questions agents asked you. Answer with one click; the waiting agent continues. |
| **Docs** | Project documents in `projects/<key>/docs/`, with the tickets that link them. |
| **Sync & health** | Each store's sync state, conflicts kept during syncs, and `ka check` findings. |

Press **c** to create a ticket, **⌘K** (Ctrl+K) to jump anywhere, and on an open ticket **s**, **p**,
**a**, **l** to change status, priority, assignee or labels, **e** to edit the description, **m** to
comment.

Agents that need you show up in the Inbox. From the terminal: `ka inbox`, then
`ka answer APP-7QK3M --choice 0` or `ka approve APP-7QK3M`.

## 6. Add a teammate

A teammate clones nothing by hand. They install `ka` and add the store to their workspace:

```bash
ka store add git@github.com:example/tickets.git
ka ls
```

`ka` keeps a managed clone, fetches before reads that need it, and replays local commits on top of
others' changes without merge commits. Edits that cannot apply are kept as comments on the
ticket, never dropped (see **Sync & health**).

## 7. Several stores

A workspace can combine stores: for example an `org` store with shared configuration, actors and
boards, plus one store per team. List them in `~/.config/ka/workspace.yml`:

```yaml
name: me
stores:
  - name: org
    url: git@github.com:example/ka-org.git
  - name: app
    url: git@github.com:example/app-tickets.git
identity:
  actor: alice
```

A store inherits configuration from another with `extends:` in its `.ka/store.yml`; `ka config
explain` shows where each setting comes from. Tickets can block tickets in other stores, and boards
span them all.

## 8. Keep it healthy

```bash
ka check                           # validate stores against the spec
ka fix                             # repair formatting, timestamps, actor aliases (one commit each)
ka doctor --fix                    # git setup: merge driver, attributes
ka hooks install                   # a pre-commit hook that formats and checks hand-written files
```

In CI, add the check action to the store repository:

```yaml
- uses: actions/checkout@v7
- uses: KanArtist/ka/actions/check@main
```

## Next

- Shell completion: `source <(ka completion bash)` (also zsh, fish, PowerShell).
- A daemon for the whole team on a server: [operations.md](operations.md).
- How it works: [engine.md](engine.md), [api.md](api.md); the file format: [the Store Spec](../spec/v1/README.md).
