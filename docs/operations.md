# Operations: running a remote daemon

A local `ka daemon` (started by `ka open` or `ka mcp`) needs no setup. This page is for running
one daemon for a team on a server. Details of authentication are in [api.md](api.md#remote-mode).

## With the container image

```bash
docker volume create ka-data

# 1. A workspace file listing the stores (managed clones go in /data/stores).
docker run --rm -i -v ka-data:/data --entrypoint sh ghcr.io/kanartist/ka -c 'cat > /data/workspace.yml' <<'YAML'
name: team
stores:
  - name: org
    url: git@github.com:example/ka-org.git
  - name: jk
    url: git@github.com:example/jk-tickets.git
YAML

# 2. One API token per person (printed once; only the hash is kept in /data/config).
docker run --rm -v ka-data:/data ghcr.io/kanartist/ka token create --actor alice

# 3. The daemon. It pushes with the deploy key mounted at ~/.ssh.
docker run -d --name ka -p 7711:7711 -v ka-data:/data \
  -v /srv/ka/ssh:/home/ka/.ssh:ro \
  -e GIT_AUTHOR_NAME="ka daemon" -e GIT_AUTHOR_EMAIL=ka@example.com \
  -e GIT_COMMITTER_NAME="ka daemon" -e GIT_COMMITTER_EMAIL=ka@example.com \
  ghcr.io/kanartist/ka daemon --listen 0.0.0.0:7711 \
    --workspace /data/workspace.yml --public-url https://ka.example.com \
    --webhook-secret-file /data/config/webhook-secret
```

The image runs as an unprivileged user (uid 10001), includes `git` and `ssh`, and keeps
everything under the `/data` volume: `stores/` (managed clones), `config/` (tokens), `run/`
(runtime file). Build it from source with `docker build -t ka .`.

Commits carry each person's identity in `Ka-Actor` trailers; the git author is the daemon's
identity. Forge branch protection still applies to the daemon's pushes.

## Behind a reverse proxy

Terminate TLS at the proxy and forward to port 7711. The event stream (`/api/v1/events`) and MCP
(`/mcp`) are long-lived responses: turn off response buffering (nginx: `proxy_buffering off;`,
`proxy_read_timeout 1h;`).

With an authenticating proxy (oauth2-proxy, Cloudflare Access, Tailscale), let it assert the user
instead of handing out tokens:

```bash
ka daemon --listen 0.0.0.0:7711 --public-url https://ka.example.com \
  --trusted-proxy 10.0.0.5 --proxy-user-header X-Forwarded-Email
```

Emails map to handles through `actors.yml` (`emails:`). The header is honored only from the
trusted addresses.

## Clients

- Browser: `https://ka.example.com/auth?token=ka_…` once, then bookmark `https://ka.example.com`.
- Agents (MCP over HTTP): endpoint `https://ka.example.com/mcp` with header
  `Authorization: Bearer ka_…`; agents are recorded as acting on the token owner's behalf.
- Scripts: the REST API with the same header (`GET /api/v1/openapi.json`).

## Freshness

The daemon fetches every store every 15 s (`fetch_interval` per store in the workspace file). For
changes pushed by others to appear within a second, add a GitHub webhook (push events) to each
store repository: URL `https://ka.example.com/api/v1/hooks/github`, content type JSON, and the
secret in `--webhook-secret-file`.

## Monitoring

- `GET /healthz` (no token) for liveness.
- `GET /metrics` (token) for Prometheus: request rates and latency, operations by result, sync
  state per store (`ka_store_sync{state="degraded"}` means fetches or pushes are failing and work
  is local only), tickets by category.
- `ka daemon -v --log-format json` logs every request as JSON.
- The web UI's **Sync & health** view shows the same per store, with preserved conflicts and
  `ka check` findings.
