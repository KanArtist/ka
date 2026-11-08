# Releasing

Releases are built by GoReleaser (`.goreleaser.yaml`) from a `v*` tag
(`.github/workflows/release.yml`). Nothing is published until a tag is pushed.

```bash
go run github.com/goreleaser/goreleaser/v2@latest check                 # validate the config
go run github.com/goreleaser/goreleaser/v2@latest build --snapshot --clean   # all six binaries into dist/
git tag v1.0.0 && git push origin v1.0.0                                 # publish (a draft release)
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

## One-time setup (not done yet)

- Create the repositories `JumpKickOSS/homebrew-tap`, `JumpKickOSS/scoop-bucket` and a fork
  `JumpKickOSS/winget-pkgs`, and a token with write access to them as the `TAP_GITHUB_TOKEN`
  secret. Without it, those three steps are skipped.
- Allow GitHub Actions to write packages (ghcr.io) in the organization settings.
- Check package-registry names for conflicts before 1.0 (PRD decision D10).

`go install github.com/JumpKickOSS/ka/cmd/ka@v1.0.0` also works and reports the tag as its version.
