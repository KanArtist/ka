# ka check action

Validates KanArtist stores in a repository against the Store Spec on every push and pull request.
Errors and warnings fail the job and appear as annotations on the files and lines.

```yaml
# .github/workflows/ka-check.yml in a store repository
name: ka check
on: [push, pull_request]
jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: KanArtist/ka/actions/check@v1.3.0 # or a full commit SHA
        with:
          version: v1.3.0
```

Reference the action by a release tag, or better by that tag's full commit
SHA, rather than `@main`: the action decides which binary runs in your CI.
Setting `version` to the same release keeps the binary from moving either.

The action downloads the release archive, checks it against the release's
`checksums.txt`, and, when `cosign` is on `PATH` (add
`sigstore/cosign-installer` before it), verifies that `checksums.txt` was
signed by this repository's release workflow at that tag. Without cosign it
says that the signature was not verified.

For code scanning instead of annotations:

```yaml
      - uses: KanArtist/ka/actions/check@v1.3.0
        id: ka
        with:
          format: sarif
        continue-on-error: true
      - uses: github/codeql-action/upload-sarif@v4
        with:
          sarif_file: ${{ steps.ka.outputs.sarif-file }}
```

Inputs:

| Input | Default | Meaning |
|---|---|---|
| `path` | `.` | Store directories, space-separated. |
| `version` | `latest` | A ka release tag, or `latest` (resolved to a tag once). If that release has no archive for the runner's platform, the action builds the same tag with `go install` (add `actions/setup-go`); any other download failure fails the step. |
| `format` | `github` | `github` annotations, or `sarif`. |
| `sarif-file` | `ka-check.sarif` | Where the SARIF report goes. |
| `skip-install` | `false` | Use the `ka` already on `PATH`. |
| `args` | | Extra `ka check` arguments, such as `--map git@github.com:org/ka-org.git=../org` when a config parent is checked out beside the store. |

Other CI systems run the same command: `ka check --ci` exits 1 on errors or warnings; add
`--format sarif -o ka.sarif` or `--format json` for machine-readable output.
