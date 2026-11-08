# Contributing

Thanks for helping. KanArtist dogfoods itself: its own tickets live in a KanArtist store.

## Build and test

```bash
go test ./...                    # everything (the S5 multi-clone simulator takes ~30 s)
go test -short -race ./...       # what CI runs on Linux, macOS and Windows
cd web && npm ci && npm run gen:api && npm run build   # the web UI, embedded into internal/api/ui
```

Requirements: Go 1.27+, git 2.40+, Node 26 for the UI. The built UI is committed so that
`go install` works without Node; CI fails if it does not match `web/`.

## Shape of a change

- **Spec first.** The Store Spec (`spec/v1/`) is the contract. A change to what is valid on disk
  updates the prose, the JSON Schemas, the conformance corpus and `pkg/kaspec` together; tests
  fail if the diagnostic-code table, defaults, schemas and validator disagree. Spec changes go
  through an RFC-style issue first.
- **One service layer.** The CLI, MCP tools, REST API and web UI call the same `engine.Service`
  methods. A new capability gets an engine method with tests, then its surfaces; a test fails if a
  service method has no REST endpoint.
- **Every write is an operation.** Changes to tickets go through `Op`s so they replay safely after
  a concurrent push. Do not text-merge ticket files.
- **Errors explain themselves.** Return `engine.Errf(code, fix, …)`: a stable code, and a fix a
  person or agent can act on.
- **Tests run against real git.** Use `internal/testkit` (bare remotes and clones); scenarios in
  `internal/scenario` are the PRD's acceptance tests.

Keep commits focused, with messages that say what changed and why.

## License

By contributing you agree that your contributions are licensed under Apache-2.0.
