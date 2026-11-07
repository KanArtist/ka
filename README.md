# KanArtist (`ka`)

A work tracker for teams whose work is mostly done by AI agents, with git as the database.
Agents are the primary users; humans get a live window into what they are doing.

- **Product requirements:** [docs/prd.md](docs/prd.md)
- **The Store Spec (the on-disk contract):** [spec/v1/README.md](spec/v1/README.md)
- **Migrating from KanArtist v0:** [docs/migration.md](docs/migration.md)

## Status: M0 (spec)

This repository currently contains milestone M0 from the PRD:

| Piece | Where |
|---|---|
| Store Spec v1: prose, built-in defaults | [`spec/v1/`](spec/v1/) |
| JSON Schemas for every file type | [`spec/v1/schema/`](spec/v1/schema/) |
| Conformance corpus (22 cases; every diagnostic code exercised) | [`spec/v1/conformance/`](spec/v1/conformance/) |
| `kaspec`: parse, canonical format, config layering, validation | [`pkg/kaspec/`](pkg/kaspec/) |
| `ka check`, `ka fmt`, `ka version` | [`cmd/ka/`](cmd/ka/) |
| One-time v0 importer (repeatable until cut-over) | [`tools/migrate-v0/`](tools/migrate-v0/) |

The engine, MCP server, daemon and web UI come in M1 to M4 ([PRD §21](docs/prd.md#21-milestones)).

## Using it

Requires Go 1.27+.

```bash
go build -o ka ./cmd/ka

ka check                       # validate the store containing the current directory
ka check ../org ../jk          # validate several stores as one workspace
ka check --strict --json       # CI: warnings fail too, machine-readable output
ka fmt                         # rewrite tickets and comments in canonical form
ka fmt --check                 # list non-canonical files; exit 1 if any
```

`extends` URLs that are local paths (`../org`) resolve relative to the extending store. Map remote
URLs to local checkouts with `--map git@github.com:org/ka-org.git=../org`.

## Development

```bash
go test ./...                                          # unit, property and conformance tests
KA_V0_CORPUS=~/src/oss/kanartist go test ./tools/...   # plus the real v0 corpus, if you have it
```

Spec changes and code changes travel together: tests fail if the diagnostic-code table in the spec,
the built-in defaults, the JSON Schemas and the Go validator disagree.

## License

[Apache-2.0](LICENSE)
