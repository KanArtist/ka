# The KanArtist web UI

Angular (zoneless, signals, standalone components). `ka daemon` serves the production build, which
is embedded in the binary from `internal/api/ui/`, so the UI works offline.

```bash
npm ci
npm run gen:api   # regenerate src/app/api/schema.ts from the daemon's OpenAPI document (needs Go)
npm run build     # build into ../internal/api/ui; commit the result
```

For development, run `ka daemon` and `npx ng build --watch --configuration development`, and
reload `ka open`. The build output is what the daemon serves, so you need no proxy.

CI rebuilds the UI and fails if the committed bundle or the generated API types are stale.

Design: graphite surfaces, with actor ink as the one loud idea. Agents act in verdigris with a
hexagonal avatar, humans in ochre with a round one, everywhere an actor appears. The tokens are in
`src/styles.css`.
