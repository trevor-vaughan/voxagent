# Performance evidence

What has been measured about voxagent's speed and memory against Ruby
Puppet, how, and what it does and does not show. Measurements only; each
section names its date, versions and method so it can be repeated.

## Synthetic `file` catalogs (2026-10-05)

Median of five; wall clock and peak RSS per run.

| Resources | Phase | Ruby | voxagent | Ruby RSS | voxagent RSS |
|---|---|---|---|---|---|
| 100 | cold | 1.538 s | 0.015 s | 72 MiB | 11 MiB |
| 100 | steady | 1.263 s | 0.008 s | 72 MiB | 9 MiB |
| 1,000 | cold | 5.604 s | 0.112 s | 100 MiB | 24 MiB |
| 1,000 | steady | 2.260 s | 0.038 s | 84 MiB | 14 MiB |
| 5,000 | cold | 28.021 s | 0.500 s | 251 MiB | 66 MiB |
| 5,000 | steady | 8.375 s | 0.215 s | 151 MiB | 28 MiB |

Method: N `file` resources (`content`, `mode`), compiled once on the host and
applied by both agents from the same bytes; five repetitions, median; wall
clock from `$EPOCHREALTIME`, peak RSS from GNU `time`; every run's report
checked for `changed` (cold) or `unchanged` (steady). Puppet 8.10.0 on the
host; voxagent built with the host's Go toolchain (`go version`: go1.26.7,
Red Hat 1.26.7-1.el10, read when this was recorded rather than during the
runs). Raw results are in the throwaway `.test-output/bench/full/results.tsv`
(not committed). RSS is kB/1024, labelled MiB. The host was shared and its
load was not recorded during these runs (a later reading, 2026-10-05 18:17,
was 29.6/46.2/54.9 over 1/5/15 minutes on 24 CPUs), so absolute times carry
unknown noise; the ratios between agents are more trustworthy than the
seconds.

Where Ruby's time went at 5,000 resources, from two separate `--debug` runs
of the steady-state apply (debug logging inflates absolute times, and the two
runs differ from each other and from the table; both outputs are throwaway
files under `.test-output/bench/full/n5000/`, not committed):

- `profile-debug.out`: `Applied catalog in 6.91 seconds`; the sum of
  `--evaltrace` `Evaluated in` over 5,008 resources is 0.38 s.
- `ts.out` (every line timestamped, 12.20 s total): 6.16 s from `catalog
  supports formats` to the first `Loaded state` (catalog ingest), and 3.59 s
  from `Applying configuration version` to the first `Finishing transaction`
  (graph and transaction work, which is simply the span between those two log
  lines).

## Representative catalog (2026-10-05)

A scratch environment (not committed) compiled on the host with Puppet 8.10.0
from `bench_profile` (300 `bench_profile::site` defines, 40 services) plus the
vendored `stdlib` 10.0.2, `concat` 10.0.1 and `simplib` 7.0.1.

Resources by type, 2,153 in total with 2,152 edges: `File` 601,
`Bench_profile::Site` 300, `Concat::Fragment` 300, `Concat_fragment` 300,
`Exec` 300, `Notify` 300, `Service` 40, `Package` 5, `Class` 3, `Stage` 1,
`Node` 1, `Concat` 1, `Concat_file` 1.

Ruby, `puppet apply --catalog --noop --modulepath <vendored modules>`, openvox
8.29.0 in a systemd-booting container (`systemctl is-system-running` reported
`running`). Five runs, wall clock around `puppet apply` from `$EPOCHREALTIME`
inside the container, with Puppet's own `Applied catalog in` figure:

| Run | Wall | Applied catalog in |
|---|---|---|
| 1 | 13.31 s | 5.15 s |
| 2 | 7.99 s | 2.82 s |
| 3 | 8.95 s | 3.27 s |
| 4 | 13.68 s | 5.23 s |
| 5 | 12.94 s | 3.72 s |

Median wall 12.94 s, median applied-catalog 3.72 s. Wall times include the
`podman exec` overhead. These five are the last of nine timed runs; the
earlier three, taken first in the same container with the same command, were
wall 15.79, 10.97 and 11.04 s (applied 5.75, 4.22, 4.02 s), and one untimed
run reported `Applied catalog in 2.67 seconds`. All nine wall times span
7.99 to 15.79 s. The host was shared (load average about 50 on 24 CPUs during
the runs), so read the spread, not any single figure.

One further run with `--debug --evaltrace`, every output line timestamped,
split at the first `Loaded state` and first `Finishing transaction`
(total 6.36 s; this single run was faster than most plain runs above, so do
not compare it with the wall times, and its spans are not comparable with the
spike's 5,000-resource spans, which used a different catalog and conditions):

| Span | Time |
|---|---|
| Process start to `catalog supports formats` (startup) | 1.02 s |
| `catalog supports formats` to first `Loaded state` (ingest) | 0.87 s |
| `Applying configuration version` to first `Finishing transaction` (graph and evaluation) | 4.33 s |
| Per-resource `Evaluated in` sum, 2,761 resources (inside the span above) | 1.73 s |

"Process start" is the first timestamped output line: Ruby's boot before
Puppet's first log line is in neither the 1.02 s nor the 6.36 s total.

2,761 evaluated against 2,153 in the catalog is the relationship graph, not
a miscount; counted from `ts.out` by resource reference and read from the
8.10.0 gem (the run was openvox 8.29.0). Puppet replaces each of the 606
containers (300 `Bench_profile::Site`, 300 `Concat::Fragment`, 3 `Class`, and
one each of `Stage`, `Node`, `Concat`) with an `admissible_` and a
`completed_` whit (`graph/relationship_graph.rb:251-252`), and a whit logs
under its container's reference with the prefix stripped (`type/whit.rb:26`),
so each container appears twice: 1,547 non-container resources plus 1,212
whits. The remaining two are `Concat_file`'s generated `File[/etc/bench.conf]`
(concat 10.0.1 `concat_file.rb` `generate`) and the `completed_/etc/bench.conf`
sentinel Puppet adds for a resource that eval-generates
(`transaction/additional_resource_generator.rb:87`), logged as
`/etc/bench.conf`.

voxagent refuses this catalog (`voxagent apply --noop`, exit 2,
`outcome=could-not-run`), measured with the host's Go
toolchain (go1.26.7, as for the table above). By count:

Defined types, refused wrongly:

```text
    300 resource type Bench_profile::Site is not implemented
    300 resource type Concat::Fragment is not implemented
      1 resource type Concat is not implemented
```

Types genuinely not implemented:

```text
    300 resource type Concat_fragment is not implemented
     40 resource type Service is not implemented
      1 resource type Concat_file is not implemented
```

The first three are defined types: the catalog marks them
`"kind":"defined_type"` (601 resources). Puppet turns every catalog-format-2
resource whose kind is not `compilable_type` into an inert `Component`
(`puppet/resource.rb:515-526`, 8.10.0), so Ruby applies nothing for them.
voxagent refusing them is a voxagent defect, not a coverage gap.

## What this does and does not show

Decision rule, set before profiling: if ingest plus graph is under half of Ruby's apply
time on this catalog, the engine thesis is weaker than the spike suggested.
On this catalog it is not under half: in the one instrumented run, ingest
(0.87 s) plus graph work (the 4.33 s span less the 1.73 s evaluation sum,
2.60 s) is 3.47 s of 6.36 s, about 55%; counting evaluation inside the span it
is 5.20 s, about 82%. That is a ratio from a single run under load noise, not
a precise share. Two caveats pull in opposite directions: the 6.36 s
denominator includes the 1.02 s (1.015 s in `ts.out`) of startup that the
numerator excludes, which lowers both shares (without it they are about 65%
and 97% of 5.35 s), while under `--noop` evaluation is cheaper than in a real
apply, which raises them; which effect dominates in a real apply was not
measured. voxagent could not be timed on this catalog because it refuses it,
so no voxagent figure exists here.
