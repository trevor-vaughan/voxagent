# `voxagent agent`: transport and identity

Maintainer notes for the one-shot agent run against an OpenVox server. Operators
read the README's "Running against an OpenVox server". The divergences from
`puppet agent` are D-59 to D-77 in [`apply-spine.md`](apply-spine.md).

## Package layout

| Path | Role |
|---|---|
| `internal/puppetapi/client.go` | HTTP client for the CA, node, catalog, file metadata/content and report endpoints. Names are path-escaped, bodies are size-capped after decompression, redirects are refused, there is no proxy, and server messages are sanitised before they reach a log or report. |
| `internal/puppetapi/tls.go` | The TLS context: verifies the server against the saved CA bundle with a hostname check, and checks revocation against the CRL for every certificate in the peer chain. |
| `internal/agent/ssl.go` | `Bootstrap`: the SSL state machine (CA, CRL, key, CSR, certificate, 24 h refresh), `--ca-fingerprint`, `--waitforcert`, and the modes of everything under ssldir. |
| `internal/agent/pluginsync.go` | `Pluginsync`: mirrors the `plugins` and `pluginfacts` mounts, checksum-verified, purging only after every fetch succeeded. |
| `internal/agent/confine.go` | Shared filesystem safety: `validRelPath`, `ensureDir` (through `os.Root`), and `checkTrusted` (ownership and write bits of a path and its parents). |
| `internal/agent/facts.go` | `Facts`: runs facter with a scratch environment and wraps its JSON in the facts form the catalog request carries. |
| `internal/agent/sources.go` | `FetchSources`: resolves every `puppet:` source of every File into a run-private directory and returns the map the RAL reads (`internal/ral` Sources). |
| `internal/agent/run.go` | `Prepare`: the sequence below, returning the catalog bytes, the client and the sources map. |
| `cmd/voxagent/agent.go` | Flags, `runAgent`: calls `Prepare`, applies, then PUTs the report. |
| `cmd/voxagent/apply.go` | `applyCatalog`, shared with `voxagent apply`, so both commands apply and exit identically. |

## Run sequence

`Prepare` runs, and any error before apply is could-not-run with
nothing applied:

1. `Bootstrap` the SSL state.
2. Node request, which yields the environment.
3. Trusted-location checks (libdir, pluginfactdest, vardir, facter), creating
   libdir and pluginfactdest 0755 if absent.
4. `Pluginsync` for that environment.
5. `Facts`.
6. Catalog POST (`static_catalog=false`). A catalog naming a different
   environment than pluginsync used is refused (D-66).
7. `FetchSources` into a directory under vardir, removed by `Prepared.Close`
   on every exit path.

`runAgent` then applies the catalog through `applyCatalog`, which fills the
report's catalog fields (environment, code id, version) as for `apply`, and
PUTs the report. Once the transaction has started the report is sent even when
the run ends could-not-run (D-62). A failed PUT is logged and leaves the exit
code alone, as Puppet's `send_report` does.

## Trust model

The agent runs as root, loads Ruby that the server sent, and runs external
facts. A wrong trust decision is remote code execution as root, so these
rules are requirements. Summary, with the reason for each:

- **One unverified request.** Only the first `certificate/ca` GET, and only
  when `ca.pem` is absent, skips verification, on a dedicated client with no
  client certificate. Puppet does the same; there is nothing to verify against
  yet. Every later request is verified against the saved bundle, with the
  server name checked.
- **Pin.** `--ca-fingerprint` is the SHA-256 of the root's DER, checked on
  first fetch, on load and on every refresh. It differs from Puppet's
  `ca_fingerprint` (D-68) because the DER of the root names the trust anchor
  itself and does not change when intermediates are added.
- **Revocation fails closed.** A missing, badly signed or expired CRL for any
  CA in a chain fails the connection. Only a failed *refresh* keeps the old
  valid file, as Puppet does. The CA refresh client does no revocation check,
  matching Puppet's `revocation: false` for CA/CRL fetches.
- **A refresh cannot change the root.** A new bundle is accepted only if it
  still holds the trusted root and every certificate in it is a CA (D-73).
- **The issued certificate is checked before it is saved:** chains to the CA,
  CN equals the certname, not a CA, in its validity window, key matches.
- **Server data never chooses a path.** Pluginsync and source writes go
  through an `os.Root`; `relative_path` is validated; link metadata is refused;
  local symlinks are replaced, not followed; purge never crosses a symlink.
- **Content is verified.** Checksums are computed while streaming and must
  match; md5, mtime, ctime and none are refused.
- **Trusted locations** and **fail-closed pluginsync** (D-64, D-65)
  keep a writable directory or a half-synced libdir from becoming root code
  execution.
- **Nothing sensitive is logged:** key, CSR, fact values, catalog body, file
  content.
- **`Sensitive` values are redacted** in the report and the logs as Puppet
  redacts them (apply-spine.md D-8), and in error text where Puppet leaks them
  (D-78). A mark that cannot apply (a plain parameter such as `cwd`) is a
  console warning, `Warning: /Exec[x]: Unable to mark 'cwd' as sensitive: …`,
  printed on the agent's own log and never put in the report, as in Puppet.
  The exception is a plain parameter's value Puppet itself does not redact: a
  marked `environment` value that a command echoes reaches the report through
  the command's `logoutput`.

Residual risk, accepted: trust on first use when `--ca-fingerprint` is not
given, and a server the operator trusts can still send root-executed Ruby by
design (that is what a Puppet server does).

## Running the gate

```bash
task fleet:images        # once: builds localhost/fleet-node-el:latest
task dev:agent-gate
```

Needs podman. It is outside `task check` because it starts an OpenVox server
and two nodes (about 30 s warm), and it builds first, because the binary is
copied into the node. The server image is pinned by digest in
`tests/agent/compose.yml`; the first run pulls it. The script is
`tests/agent/run.sh`; its header lists the assertions. Exit 0 means every
assertion held, 1 an assertion failed, 2 it could not run (never a pass).
Artifacts, including both agents' reports, are in `.test-output/agent/`.
Its place among the other gates is the table in `apply-spine.md`.

The reports are compared through `tests/spine/allowlist.json` merged with
`tests/agent/allowlist.json` (run.sh writes the merged copy to
`.test-output/agent/allowlist.json` and refuses a key both files name). The
gate's own file holds only what an agent run adds: the fields that vary
between two agent runs on one node (the catalog UUID, the agent phase
timings), the node identity two different nodes cannot share, and the
D-rows only an agent run shows (D-67, D-75, D-76). Each entry says why.
