# Releasing

Releases are built by GoReleaser (`.goreleaser.yaml`) from a `v*` tag
(`.github/workflows/release.yml`). Nothing is published until a tag is pushed.

```bash
go run github.com/goreleaser/goreleaser/v2@latest check                 # validate the config
go run github.com/goreleaser/goreleaser/v2@latest build --snapshot --clean   # all six binaries into dist/
git tag v1.0.0-rc.1 && git push origin v1.0.0-rc.1                       # a prerelease: no tap, bucket or winget updates
git tag v1.0.0 && git push origin v1.0.0                                 # a release
```

A release contains:

- `ka_<os>_<arch>.tar.gz` (`.zip` on Windows) for linux, darwin and windows on amd64 and arm64,
  built with `CGO_ENABLED=0` and `-trimpath`, version stamped with `-X main.version`. The names
  carry no version, so `releases/latest/download/ka_linux_amd64.tar.gz` always works (the
  `actions/check` action uses it).
- `checksums.txt`, signed keylessly with cosign (`checksums.txt.sig`, `.pem`), and an SPDX SBOM per
  archive. Verify with:
  `cosign verify-blob --certificate checksums.txt.pem --signature checksums.txt.sig --certificate-identity-regexp 'github.com/JumpKickOSS/ka' --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt`
- `.deb` and `.rpm` packages (depending on `git`).
- `ghcr.io/jumpkickoss/ka:<version>` (and `latest`) for linux/amd64 and arm64, signed with cosign.
- Homebrew cask (`JumpKickOSS/homebrew-tap`), Scoop manifest (`JumpKickOSS/scoop-bucket`) and a
  winget pull request (from `JumpKickOSS/winget-pkgs` to `microsoft/winget-pkgs`).

## Package-manager credentials

Each package manager is skipped when its secret is unset, and for prereleases.

| Secret | What it is | Repository |
|---|---|---|
| `TAP_SSH_KEY` | Private half of a write deploy key ("goreleaser") | `JumpKickOSS/homebrew-tap` |
| `SCOOP_SSH_KEY` | Private half of a write deploy key ("goreleaser") | `JumpKickOSS/scoop-bucket` |
| `WINGET_GITHUB_TOKEN` | A fine-grained token with contents and pull-request write on the fork | `JumpKickOSS/winget-pkgs` (a fork of `microsoft/winget-pkgs`) |

Deploy keys can push to one repository only, so a leaked key cannot touch anything else. To
rotate one:

```bash
ssh-keygen -t ed25519 -N "" -C goreleaser -f key
gh repo deploy-key add key.pub -R JumpKickOSS/homebrew-tap --allow-write -t goreleaser
gh secret set TAP_SSH_KEY -R JumpKickOSS/ka < key && rm key key.pub
```

The winget token has to be created in the browser (Settings → Developer settings →
Fine-grained tokens, resource owner JumpKickOSS, repository `winget-pkgs`), then
`gh secret set WINGET_GITHUB_TOKEN -R JumpKickOSS/ka`. A new package's first winget pull request
is reviewed by the winget maintainers before it merges.

`go install github.com/JumpKickOSS/ka/cmd/ka@v1.0.0` also works and reports the tag as its version.
