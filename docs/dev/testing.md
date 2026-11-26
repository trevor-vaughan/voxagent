# Testing

For contributors and maintainers. Back to the [developer docs](README.md).


```bash
task test              # human-readable output
task test MODE=llm     # failures only
task integration:venom # end-to-end against a real Puppet; needs `venom` and `puppet` on PATH
task check             # all quality gates
```

Artifacts are written to `.test-output/`; `task integration:venom` writes the Venom results there.
These commands need [Task](https://taskfile.dev) installed. The task does not provision Puppet or Venom.

## Continuous integration

GitHub Actions runs the workflows below.

[`ci.yml`](../../.github/workflows/ci.yml) runs on every push to `main` and every pull request:

| Job | What it runs |
| --- | --- |
| `task check` | `go mod tidy -diff`, `task check`, then `task integration:venom` |
| `go test -race` | the unit tests under the race detector |
| `govulncheck` | known-vulnerability reachability, against the CI toolchain |
| `golangci-lint` | default linters, **new issues only**; existing findings are a backlog to fix |

CI builds with the newest Go 1.26 patch, not the `go 1.26.5` floor in `go.mod`, because 1.26.5 carries reachable
standard-library vulnerabilities. Ruby with the `puppet` and `facter` gems, at the versions pinned in the `env`
block of `ci.yml`, is the oracle for the extension-host tests and the report differentials; moving those versions is
a re-measurement, not a routine bump.

[`gates-containers.yml`](../../.github/workflows/gates-containers.yml) runs weekly and on demand (`workflow_dispatch`).
It builds the fleet images and runs `dev:state-diff`, `dev:report-diff-container`, `dev:sensitive-canary`,
`dev:augeas-lib` and `dev:agent-gate`, one job each. `dev:state-diff` and `dev:report-diff-container` refuse to run
(exit 2, a failed job) when the freshly built image's openvox differs from their `WANT_PUPPET` pin; see
[Gates](gates.md). The systemd, SIMP and
SELinux gates are not run in CI: they need a delegated cgroup or a QEMU VM.

Each workflow uploads `.test-output/` as an artifact: `ci.yml` on failure, the container gates always.
Dependabot keeps the commit-SHA-pinned actions current. The tool versions pinned inside the workflows (govulncheck,
golangci-lint, Venom) are bumped by hand.

### Dependabot auto-merge

[`dependabot-auto-merge.yml`](../../.github/workflows/dependabot-auto-merge.yml) turns on GitHub auto-merge for
Dependabot's patch and minor updates whose branch lives in this repository; majors and fork PRs wait for a person.
Dependabot only proposes a release once it has aged past the `cooldown` in
[`dependabot.yml`](../../.github/dependabot.yml). Two repository settings make this safe, and
neither lives in the repository:

1. Settings → General → **Allow auto-merge**.
2. A branch ruleset on `main` that requires the `ci.yml` checks (`task check`, `go test -race`, `govulncheck`,
   `golangci-lint`). Without required checks, auto-merge merges a PR as soon as it is mergeable, before CI finishes.

