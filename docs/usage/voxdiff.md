# voxdiff

For operators. Back to the [usage index](README.md).


Compiles a Puppet catalog twice and reports any divergence, ignoring the two
fields that legitimately vary between compiles: `version` and `catalog_uuid`.

## Build

```bash
task build
```

## Compare two compiles

```bash
./bin/voxdiff compare \
  -node web01.example.com \
  -environmentpath /etc/puppetlabs/code/environments \
  -environment production
```

Exit codes: `0` identical, `1` diverged, `2` operational failure. The
distinction matters in CI — "the catalogs differ" is a result, "the compile
broke" is not.

Add `-json` for machine-readable output, and `-ignore-source-locations` when
comparing across a manifest reorganisation.

## Compare two implementations

`-left` and `-right` select the puppet binary that compiles each side. Both
default to `puppet`, so without them the same binary is compared against
itself. This example compiles with `puppet` on one side and `/usr/bin/openvox`
on the other:

```bash
./bin/voxdiff compare -node web01 -left puppet -right /usr/bin/openvox
```

`-environmentpath` is not set here, so both binaries use their own default,
and `-environment` defaults to `production`. Add the flags from the first
example to compare a specific environment.

## Scan for non-deterministic constructs

```bash
./bin/voxdiff scan /etc/puppetlabs/code/environments/production/modules
```

Flags exported resources, exported collectors, `generate()` calls, and I/O in
custom functions — the cases where compile determinism stops holding.

Exit codes: `0` clean, `1` findings, `2` incomplete — a file could not be
read or scanned, so the tree was not fully reviewed and the result should
not be trusted. Exit `2` can happen alongside real findings on stdout: an
error on one file does not stop the scan of the rest, so treat exit `2` as
"re-run after fixing access, don't just read the findings" rather than a
plain crash. Findings always go to stdout; errors always go to stderr.
