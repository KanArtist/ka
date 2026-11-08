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
      - uses: JumpKickOSS/ka/actions/check@main
```

For code scanning instead of annotations:

```yaml
      - uses: JumpKickOSS/ka/actions/check@main
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
| `version` | `latest` | A ka release tag. Before the first release, the action builds ka with `go install` (add `actions/setup-go`). |
| `format` | `github` | `github` annotations, or `sarif`. |
| `sarif-file` | `ka-check.sarif` | Where the SARIF report goes. |
| `args` | | Extra `ka check` arguments, such as `--map git@github.com:org/ka-org.git=../org` when a config parent is checked out beside the store. |

Other CI systems run the same command: `ka check --ci` exits 1 on errors or warnings; add
`--format sarif -o ka.sarif` or `--format json` for machine-readable output.
