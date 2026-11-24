# Differential harness internals

For maintainers. Back to the [README](../../README.md).

## Why this exists before any agent code

The harness is the instrument that turns "the Go agent behaves correctly" into a
measurement. Building it first means every later claim about agent fidelity is
backed by a number rather than an argument.

## Why the allowlist is exactly two fields

Three compiles of one manifest — exercising hash iteration, a defined type,
resource defaults, class inheritance with a resource override, a virtual
resource collector, and a collector-based override — produced byte-identical
output across all 20 resources and 19 edges, apart from `version` (an epoch
timestamp) and `catalog_uuid` (random per compile).

The list is `AllowlistedFields` in `internal/catalog/normalize.go`. It is measured, not assumed. Any addition needs the same evidence, and
every addition weakens the harness.

## Why resource order is compared

`lib/puppet/resource/catalog.rb:561` hardcodes `Puppet::Graph::SequentialPrioritizer`,
which derives apply-order tie-breaks from catalog position. The `ordering`
setting that once made this configurable no longer exists.

So resource order is behavioural, and a differ that sorted resources to
"normalize" them would hide real divergence. `Normalize` deliberately leaves
order alone, and `Diff` reports reordering as `KindReordered`.

Order is compared only over resources present on both sides, so a pure addition
or removal is not also reported as a reorder.

## Why tags are compared as a set

Puppet treats tags as membership — `tag ==` collectors and `--tags` filtering are
set tests — so tag order carries no behaviour and comparing it ordered would
produce noise.

## Why report output is sorted

Go randomises map iteration. Parameter keys are sorted before iteration so the
report is byte-stable across runs, which is what lets CI diff harness output.
`TestDiffOutputIsDeterministic` guards this.

The test's fixture is deliberately asymmetric in a specific way: both the left
and right resources carry multi-key parameter maps
(`internal/catalog/diff_test.go`, in `TestDiffOutputIsDeterministic`), not
just one side.

An earlier version of the test only put multiple keys on
the right side, leaving the left with a single key — a map with one key is
trivially "sorted" no matter what Go's iteration order does, so that version
could pass even if the key sort in the left-hand loop was broken.

Both sides now
carry `a`..`d` (shared, so they exercise the `KindChanged` left-hand loop) and
the right carries an additional `w`..`z` (right-only, so they exercise the
`KindOnlyInRight` right-hand loop). Collapsing either side back to a single
key silently weakens this test back to the version that always passed. Keep
both multi-key.

## Why `ExtractJSON` matches at line start

Puppet writes `Notice: Compiled catalog for ... in N seconds` to stdout, with
ANSI escapes, before the JSON body. Scanning for the first line that *begins*
with `{`, rather than the first `{` anywhere, means a brace inside the Notice
text cannot be mistaken for the body.

## Known limitations

### `scan`'s error policy: exhaustiveness outranks findings

`scan` (`cmd/voxdiff/main.go`'s `runScan`, backed by `internal/instability/scan.go`)
backs the harness's completion gate (quoted in [fleet.md](fleet.md)): "`voxdiff scan` has been run across the fleet's
module tree, and every finding has been reviewed." That sentence only holds if
`scan` actually looked at every file. Two things follow from that:

- **A file `scan` cannot read or cannot fully scan is an error, not a silent
  gap.** `filepath.WalkDir` does not abort the walk on a per-file error —
  a fleet tree has mixed ownership, and one unreadable file must not throw
  away findings already gathered from every other file — but every such error
  is still accumulated and printed to stderr, separately from the findings on
  stdout, so tooling that parses findings does not have to filter errors out
  of that stream.
- **Any accumulated error forces exit 2, even when findings were also produced.**
  - Exit 1 means "the scan completed and found something"; exit 0 means "the scan completed and found nothing." Neither is true when a file was skipped, so reporting exit 1 in that case would let an incomplete scan masquerade as a trustworthy one.
  - Exit 2 is the only code that says "do not treat this run as having reviewed the whole tree."
  - Do not weaken this to "exit 2 only if there are zero findings" — that would silently downgrade a real incompleteness signal back into the case it exists to prevent.

### The 1 MiB per-line scan limit

`scan` reads each file with `bufio.Scanner`, which errors out (`bufio.ErrTooLong`)
on a single line longer than its 1 MiB buffer — for example, a `.pp` file with
a very large single-line string literal or a generated data blob pasted
inline. That error is now surfaced through the error policy above (stderr,
exit 2) rather than silently truncating the scan, but the practical
consequence is the same either way: operators pointing `scan` at a fleet tree
with unusually large generated manifests may need to identify and either split
or exclude those files, since `scan` has no flag to raise the buffer.

### Duplicate resource refs

`indexResources` (`internal/catalog/diff.go`) keys resources by `Type[Title]`.
If two resources on the same side ever shared a ref, the map would silently
keep the last one and the diff would compare a catalog it never mentions
dropping resources from — no error, no `KindOnlyInLeft`/`KindOnlyInRight`
entry, just a quiet undercount.

This cannot arise today: both sides are compiled by Puppet, and Puppet
enforces ref uniqueness at compile time. It becomes a live concern once a
second, independent implementation is under test, whose ref-uniqueness
guarantee is exactly the kind of invariant the differential gates ([gates.md](../dev/gates.md)) exist to
check. Revisit `indexResources` when that implementation lands; do not assume
the comment above `indexResources` in `internal/catalog/diff.go` is still accurate once a non-Puppet compiler is on either
side.

### `left`/`right` are omitted-when-nil in JSON output

`Difference.Left` and `Difference.Right` (`internal/catalog/diff.go`) use
`omitempty`, so a genuinely nil value — Puppet's `undef`, or a `null` inside an
array or hash parameter — serialises identically to a field that was never
set. JSON consumers of `voxdiff compare -json` cannot distinguish "this side
was nil" from "this side was absent" by checking for the key. `Kind` is always
present and is the field to key off instead.

## Test tooling

### `scripts/run-tests.sh` exit-code contract

The script runs `go test`, writes the full log to `$TEST_DIR/go-test.log`, and
in `MODE=llm` greps that log down to failure lines only. It always exits with
the real `go test` return code, and it deliberately does not use grep as a
bare statement to get there.

An earlier version ran the grep as a plain statement under `set -euo pipefail`:

```bash
grep -E '^(--- FAIL|FAIL|panic:)|_test\.go:[0-9]+:' "$LOG"
exit "$rc"
```

`grep` exits 1 when it finds no match — a normal, expected result when the
patterns don't happen to appear, not a script error. Under `set -e` that exit
status killed the script before it reached `exit "$rc"`, so a `go test`
failure whose log didn't match the grep patterns (a build error, a panic
message worded differently than expected, tooling output) produced **no
output and the wrong exit code** in `MODE=llm` — silent failure, exactly the
case an LLM-facing failure mode has no way to notice.

The grep now runs as the condition of an `if`, where `set -e` is exempted by
POSIX shell semantics, and falls back to printing the whole log when nothing
matches:

```bash
if ! grep -E '^(--- FAIL|FAIL|panic:)|_test\.go:[0-9]+:' "$LOG"; then
  cat "$LOG"
fi
```

This looks less minimal than the bare statement. Resist "simplifying" it back
— the bare form is the version that silently swallowed failures.

### `ABS_TEST_DIR`, not `TEST_DIR`, in the `.taskfiles` includes

`.taskfiles/dev.yml`, `.taskfiles/integration.yml` and `.taskfiles/fleet.yml` each compute
`ABS_TEST_DIR: '{{.ROOT_DIR}}/{{.TEST_DIR | default ".test-output"}}'` rather
than naming that variable `TEST_DIR`. This is not a style choice.

Task resolves variables in two passes. A self-referencing template —
`TEST_DIR: '{{.ROOT_DIR}}/{{.TEST_DIR}}'` — gets evaluated again against its
own prior output on the second pass, doubling the prefix to
`/workspace/voxagent//workspace/voxagent/.test-output` when no CLI override is
given. Passing `TEST_DIR=` on the command line masks the bug, because the
override short-circuits the self-reference — which is why it is easy to test
this once, see it work, and not notice the default path is broken. Giving the
computed variable a distinct name (`ABS_TEST_DIR`) breaks the self-reference
entirely.

The remaining limitation: `TEST_DIR` is always interpreted relative to
`ROOT_DIR`. An absolute override such as `TEST_DIR=/tmp/custom` is not used
as-is — it is silently rebased under `ROOT_DIR`, producing something like
`/workspace/voxagent/tmp/custom`. There is no current escape hatch for a truly
absolute, non-repo-relative test output directory.

## Deferred components

One component of the design is deferred, because it produces no meaningful result
before its precondition:

- **`spec-runner`** — reports pass rate for ported Go components. Lands once components are
  ported to Go, which is itself conditional on D7.
