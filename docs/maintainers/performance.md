# Performance evidence

For maintainers. Back to the [README](../../README.md).

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
runs).

Raw results are in the throwaway `.test-output/bench/full/results.tsv`
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

When measured, voxagent refused this catalog (`voxagent apply --noop`, exit 2,
`outcome=could-not-run`), with the host's Go
toolchain (go1.26.7, as for the table above). By count:

Defined types, refused wrongly (`Preflight` now skips them as containers):

```text
    300 resource type Bench_profile::Site is not implemented
    300 resource type Concat::Fragment is not implemented
      1 resource type Concat is not implemented
```

Types not implemented then (`Service`, `Concat_file` and `Concat_fragment` now have native units):

```text
    300 resource type Concat_fragment is not implemented
     40 resource type Service is not implemented
      1 resource type Concat_file is not implemented
```

The first three are defined types: the catalog marks them
`"kind":"defined_type"` (601 resources). Puppet turns every catalog-format-2
resource whose kind is not `compilable_type` into an inert `Component`
(`puppet/resource.rb:515-526`, 8.10.0), so Ruby applies nothing for them.
voxagent refusing them was a voxagent defect, not a coverage gap, and is fixed.

## What this does and does not show

Decision rule, set before profiling: if ingest plus graph is under half of Ruby's apply
time on this catalog, the engine thesis is weaker than the spike suggested.
On this catalog it is not under half: in the one instrumented run, ingest
(0.87 s) plus graph work (the 4.33 s span less the 1.73 s evaluation sum,
2.60 s) is 3.47 s of 6.36 s, about 55%; counting evaluation inside the span it
is 5.20 s, about 82%.

That is a ratio from a single run under load noise, not
a precise share. Two caveats pull in opposite directions: the 6.36 s
denominator includes the 1.02 s (1.015 s in `ts.out`) of startup that the
numerator excludes, which lowers both shares (without it they are about 65%
and 97% of 5.35 s), while under `--noop` evaluation is cheaper than in a real
apply, which raises them; which effect dominates in a real apply was not
measured. voxagent could not be timed on this catalog because it refuses it,
so no voxagent figure exists here.

## Public SIMP catalog (2026-10-06)

The catalogs above are synthetic. This one is compiled from SIMP's published
modules, so its resources are the ones a SIMP node really gets, though for a
scratch node rather than a customer's.

**Environment** (scratch, not committed). In order:

1. Run `puppet module install simp-simp` in `localhost/fleet-node-el` (openvox
   8.29.0), because the host Ruby cannot fork under Puppet's tar extraction.
2. Install every module's `simp.optional_dependencies` until none is missing,
   along with the `*_core` type modules that Puppet 8 split out. This gave 77
   modules.
3. Pin one module: `simp_firewalld` 3.0.0 does not satisfy `iptables` 7.1.2's
   `>= 0.1.3 < 2.0.0`, so install 1.0.1.
4. Write `site.pp` and Hiera. `site.pp` is `include simp` with
   `simp::scenario: simp` and Hiera `simp_options` for a node without LDAP or
   PKI (`ldap`, `pki`, `stunnel`, `tcpwrappers`, `fips`, `libkv` and `clamav`
   false; `firewall`, `auditd`, `selinux`, `sssd`, `pam`, `haveged`,
   `logrotate` and `syslog` true).
5. Install `openssh-server` on the host, because `ssh::server::params` needs the
   `openssh_version` fact.
6. Compile on the host with Puppet 8.10.0 against the host's own facts (CentOS
   Stream 10, which `simp` 9.0.1 supports).

Key versions: `simp` 9.0.1,
`simplib` 4.14.2, `stdlib` 9.7.0, `concat` 9.1.0, `inifile` 6.5.0,
`augeasproviders_core` 4.2.0, `augeasproviders_sysctl` 4.1.0,
`augeasproviders_ssh` 7.0.0, `augeasproviders_grub` 6.0.1, `svckill` 5.1.1.

Resources: 725, with 746 edges. By kind, 460 `compilable_type`, 136
`defined_type`, 124 `unknown` (classes) and 5 `class`. The largest types are
`File` 146, `Class` 129, `Sysctl` 63, `Ssh_config` 49, `Package` 40,
`Sshd_config` 36, `Ini_setting` 20, `Concat_fragment` 20, `Service` 18,
`Augeas` 18 and `Exec` 11.

### What voxagent refuses

This section records the 2026-10-06 run. Native units added since serve `Sysctl`,
`Ssh_config`, `Sshd_config`, `Sshd_config_subsystem`, `Concat_fragment`,
`Concat_file`, `Augeas`, `Mount`, `Cron`, `Group`, `User` and `Host`, so the
counts below no longer describe what voxagent refuses. Of the types listed,
`Ini_setting`, `Anchor`, `Reboot_notify`, `Tidy`, `Kernel_parameter`, `Runlevel`,
`Selinux_state`, `Simp_file_line`, `Svckill` and `Init_ulimit` have no native unit
(`voxagent units` lists the ones that do).

`voxagent apply --noop`, exit 2: `293 gap(s) across 277
resource(s); nothing applied`. No defined-type instance is refused.

244 resources are refused by type. Only 8 of them are Puppet built-in types;
the other 236 are Ruby types shipped in modules:

| Resources | Type | Defined in |
|---|---|---|
| 63 | `Sysctl` | `augeasproviders_sysctl` |
| 49 | `Ssh_config` | `augeasproviders_ssh` |
| 36 | `Sshd_config` | `augeasproviders_ssh` |
| 20 | `Concat_fragment` | `concat` |
| 20 | `Ini_setting` | `inifile` |
| 18 | `Augeas` | `augeas_core` |
| 6 | `Concat_file` | `concat` |
| 6 | `Mount` | `mount_core` |
| 4 | `Anchor` | `stdlib` |
| 4 | `Cron` | `cron_core` |
| 4 | `Group` | Puppet |
| 3 | `User` | Puppet |
| 2 | `Reboot_notify` | `simplib` |
| 1 | `Tidy` | Puppet |
| 1 each | `Host`, `Kernel_parameter`, `Runlevel`, `Selinux_state`, `Simp_file_line`, `Sshd_config_subsystem`, `Svckill`, `Init_ulimit` | `host_core`, `augeasproviders_grub`, `simplib`, `selinux`, `simplib`, `augeasproviders_ssh`, `svckill`, `simplib` |

Augeas-backed types (`Sysctl`, `Ssh_config`, `Sshd_config`, `Augeas`,
`Kernel_parameter`, `Sshd_config_subsystem`) account for 168 of the 244.

33 `File` resources are refused for attributes or values rather than type:
`force` 13, `selinux_ignore_defaults` 13, `seltype` 6, `show_diff` 5,
`target` 3, `ensure => link` 2, `ensure => symlink` 1, and one each of
`replace`, `selrange`, `selrole`, `seluser`, `links` and `path`. The other
113 `File`s, all 40 `Package`s, all 18 `Service`s and all 11 `Exec`s pass
pre-flight.

### Ruby's cost

`puppet apply --catalog --noop --modulepath <modules>`, openvox 8.29.0, in
`localhost/voxagent-systemd` booted with systemd as PID 1 (`running`), the
same method as the representative catalog. Before timing, the catalog's own
packages were installed into that container, along with `procps-ng`, `grubby`,
`selinux-policy-targeted` and `kmod`, because without them Ruby's run was
degraded: all 63 `Sysctl` were skipped ("provider prefetch failed") and
`cron`, `kernel_parameter` and `selinux_state` had no suitable provider.
`haveged` (EPEL), `dracut-fips*` and `iptables-services` were left out.
Afterwards 7 resources still error, for reasons specific to the container: six
`Sysctl` keys are not visible in the container's network namespace, and
`Reboot_notify`'s `post_resource_eval` fails.

| Run | Wall | Applied catalog in |
|---|---|---|
| 1 | 15.09 s | 8.13 s |
| 2 | 15.13 s | 8.31 s |
| 3 | 15.21 s | 8.37 s |
| 4 | 15.70 s | 8.63 s |
| 5 | 15.02 s | 8.27 s |

Median wall 15.13 s, median applied-catalog 8.31 s, measured with the
`podman exec` overhead and a load average of about 16 on 24 CPUs. Unlike the
representative catalog's runs, these spread by less than 5%.

One `--debug --evaltrace` run, every line timestamped, split at the same
markers as above (total 15.26 s; `Applied catalog in 8.84 seconds`):

| Span | Time |
|---|---|
| `podman exec` to Puppet's first output line | 1.78 s |
| First line to `catalog supports formats` (startup) | 2.17 s |
| `catalog supports formats` to first `Loaded state` (ingest) | 2.44 s |
| `Applying configuration version` to first `Finishing transaction` (graph and evaluation) | 8.78 s |
| Per-resource `Evaluated in` sum, 1,025 lines (inside the span above) | 7.89 s |

Evaluation by type, from the same run:

| Time | Share | Resources | Type |
|---|---|---|---|
| 5.41 s | 68.6% | 1 | `Svckill` |
| 0.67 s | 8.5% | 40 | `Package` |
| 0.62 s | 7.9% | 18 | `Service` |
| 0.51 s | 6.5% | 63 | `Sysctl` |
| 0.47 s | 6.0% | 18 | `Augeas` |
| 0.21 s | 2.6% | the rest | |

### What this shows

On this catalog the decision rule comes out the opposite way from
the representative catalog: ingest (2.44 s) plus graph work (8.78 − 7.89 =
0.89 s) is 3.33 s, about 25% of the 13.48 s from Puppet's first line to the
end, and well under half by any choice of denominator. Ruby's time here is
spent evaluating resources, and most of that is one resource: `Svckill`
enumerating every service on the host. Excluding it, evaluation is 2.48 s,
which is still less than ingest plus graph.

So the engine's share depends
heavily on the catalog. The synthetic catalogs were dominated by many cheap
resources; a real SIMP node runs a few hundred cheap resources and a handful
of expensive ones. This is one noop run, of one scenario, in a container that
is not a real host, and evaluation would cost more in a real apply.

For coverage, the result is that the remaining gaps on a real SIMP catalog
are almost entirely Ruby types shipped in modules, especially the
augeasproviders family. Tier 1 (native) types (`user`, `group`, `tidy`) would clear 8
resources; the `File` attribute gaps would clear 33. voxagent was not timed,
because it refuses the catalog.

## Public SIMP catalog applied (2026-10-08)

The section above could not time voxagent, because voxagent refused the
catalog. With the [extension host](../usage/extension-host.md) it applies it, so
this section times both agents applying the catalog from the SIMP acceptance
gate (see [The SIMP gate](gate-evidence.md#the-simp-gate)), for real.

**Environment** (scratch shell loop, not committed). In order:

1. Build voxagent with `task dev:build`, at the first state where the gate
   agreed with every exception measured.
2. Compile the catalog that `tests/spine/report-diff-simp.sh` builds: the pinned
   modules of `tests/spine/simp/modules.pin` and the fixture beside it, in a
   booted `localhost/voxagent-simp` container (openvox 8.29.0) with that
   container's facts. It has 737 resources, 63 `Sysctl` and 16 `Binary` values.
3. For each run, boot a fresh container with `simp_boot` and apply once with
   `simp_apply` from `tests/spine/simp/lib.sh`, so each agent starts from the
   same files a pluginsynced agent would.
4. Run one at a time, alternating Ruby and voxagent.

Wall time is measured inside the container around the agent's command, so unlike
the section above it excludes `podman exec` overhead. `Applied catalog in` is read from each report's log. The host had 24 CPUs,
and other work was running on it: the 1-minute load average was 5.3 to 10.0
across these runs, against 20 to 48 in the prototype that first measured this.

Each report holds 340 resource statuses, and on both sides the same 71 fail,
for reasons specific to the container: 62 `Sysctl`, 5 `Mount`, 2
`Kernel_parameter`, 1 `Augeas` and `Package[haveged]`, which is in EPEL, and
the image does not enable EPEL. A failed resource costs less to evaluate than
one that converges, so these timings cover a catalog in which a fifth of the
statuses fail.

"Tier 3 types" in the table are the types voxagent sends to the host, which is
every type no native unit serves (`voxagent units` lists the types that are
served natively, so the host's types are the rest). Ruby's
column for them sums the same `metrics.time` entries from Ruby's own report.

| Run | Load before/after | Wall | Applied catalog in | Host start | Host-evaluated | Spent in host | Tier 3 `metrics.time` sum | Native `metrics.time` sum |
|---|---|---|---|---|---|---|---|---|
| Ruby 1 | 7.26/7.80 | 30.01 s | 24.17 s | — | — | — | 8.22 s | 13.38 s |
| Ruby 2 | 6.90/5.28 | 28.68 s | 22.80 s | — | — | — | 8.58 s | 11.89 s |
| Ruby 3 | 6.85/7.52 | 27.71 s | 21.93 s | — | — | — | 8.19 s | 11.37 s |
| Ruby 4 | 6.65/9.99 | 27.47 s | 21.43 s | — | — | — | 8.17 s | 11.19 s |
| Ruby 5 | 7.58/9.74 | 29.50 s | 23.65 s | — | — | — | 8.47 s | 12.72 s |
| voxagent 1 | 7.25/6.98 | 32.70 s | 28.66 s | 0.855 s | 244 | 12.038 s | 8.69 s | 6.55 s |
| voxagent 2 | 5.28/6.85 | 24.23 s | 20.38 s | 0.710 s | 244 | 11.945 s | 8.64 s | 5.76 s |
| voxagent 3 | 8.84/6.71 | 24.27 s | 20.37 s | 0.710 s | 244 | 11.955 s | 8.60 s | 5.70 s |
| voxagent 4 | 9.35/7.58 | 25.68 s | 21.78 s | 0.710 s | 244 | 11.912 s | 8.51 s | 5.82 s |
| voxagent 5 | 9.74/8.86 | 25.10 s | 21.18 s | 0.738 s | 244 | 11.671 s | 8.33 s | 6.42 s |

The host columns come from voxagent's `extension host: started in …,
evaluated N resource(s), … spent in the host` line. voxagent sends 246
resources to the host, and in every run two of them,
`Reboot_notify[audit:all]` and `Reboot_notify[fips]`, are skipped for failed
dependencies, which leaves 244 evaluated.

Medians of 5 runs each:

| | Ruby | voxagent |
|---|---|---|
| Wall | 28.68 s | 25.10 s (−12%) |
| Applied catalog in | 22.80 s | 21.18 s (−7%) |
| Wall − applied (start-up and ingest) | 5.85 s | 3.90 s, host start included |
| Host start-up | — | 0.710 s (0.710–0.855 s) |
| Host overhead (spent in host − Tier 3 sum) | — | 3.35 s (3.31–3.40 s) |
| Host overhead per host-evaluated resource | — | 13.7 ms (13.56–13.94 ms) |
| Tier 3 `metrics.time` sum | 8.22 s | 8.60 s (+5%) |
| Native `metrics.time` sum | 11.89 s | 5.82 s |
| Native, without `Package[haveged]` | 6.46 s | 5.47 s |

The host overhead is everything the host spends outside the Tier 3 resources'
own evaluation: `load` (`from_data_hash` and `to_ral` of all 737 resources,
plus generation), relationships, prefetch, `finish` and the framing. So 13.7 ms
per resource is an upper bound on what the protocol costs. It does not measure
that cost directly. It varied by 0.3 ms across the five runs. The prototype's figure was about
16 ms, on its probe catalog and at a load average of 20 to 48.

The five largest Tier 3 types, by Ruby's time (median of 5 runs, with the
range):

| Type | Resources | voxagent, in the host | Ruby |
|---|---|---|---|
| `Svckill` | 1 | 4.173 s (4.084–4.343) | 4.248 s (4.183–4.410) |
| `Ssh_config` | 49 | 1.380 s (1.333–1.411) | 1.252 s (1.231–1.307) |
| `Sshd_config` | 36 | 1.245 s (1.160–1.318) | 1.120 s (1.085–1.225) |
| `Sysctl` | 63 | 0.645 s (0.572–0.688) | 0.648 s (0.603–0.649) |
| `Augeas` | 18 | 0.410 s (0.347–0.467) | 0.383 s (0.343–0.448) |

These timings predate the native units for `Ssh_config`, `Sshd_config`, `Sysctl`
and `Augeas` (`augeasproviders_ssh` 7.0.0, `augeasproviders_sysctl` 4.1.0 and
`augeas_core` 2.1.0, the versions pinned in `tests/spine/simp/modules.pin`), so
of these five only `Svckill` is still sent to the host.

`Ssh_config` is the only one whose two ranges do not overlap. It is 0.13 s, or
about 10%, slower in the host, which is 2.6 ms for each of its 49 resources.

The native sum is not a like-for-like comparison, because of one resource.
Ruby's `Package[haveged]` takes 4.8 to 7.1 s of `evaluation_time` across its
five runs. voxagent's takes 0.27 to 0.35 s, for the same failing
`/usr/bin/dnf -y install haveged`. The install is timed on both sides; what
moves is the mirror-metadata fetch. voxagent's `package` prefetch, which runs
before the walk and outside every resource's timer, runs `dnf -y check-update`
because the catalog's two `latest` packages are installed, and that fetches the
metadata. Ruby's prefetch reads only the installed set, so its first `dnf`
command, haveged's install, pays the fetch.

Measured in a booted container
from a cold cache, 3 runs each: the install alone took 5.45 to 8.75 s, while
`check-update` took 3.98 to 11.09 s and the install after it 0.29 to 0.34 s.
[apply-spine.md](apply-spine.md) D-57 records this.

Without that resource, voxagent's native
types take 5.47 s against Ruby's 6.46 s. By type: `file` takes 0.082 s against
0.231 s, and `service` 3.375 s against 3.784 s. `package` without haveged
takes 2.10 s against 2.36 s. The fetch still counts in voxagent's applied
time, so that comparison is unaffected.

### A catalog with no Tier 3 type

`native-only.py` (run by hand as a scratch
filter, not committed) cut the same catalog down to the 485 resources voxagent applies
natively, together with their containers. Relationships to the removed
resources were dropped. voxagent applied that catalog five times with each set
of flags, in fresh containers, rotating through the three. The 1-minute load
average was 6.5 to 11.5.

| Flags | Walls | Median |
|---|---|---|
| default | 11.98, 11.81, 12.58, 14.54, 13.68 s | 12.58 s |
| `--no-ruby` | 11.02, 15.35, 12.91, 11.57, 11.39 s | 11.57 s |
| `--ruby /nonexistent/ruby` | 20.76, 13.86, 20.21, 12.28, 13.13 s | 13.86 s |

None of the 15 runs prints an `extension host` line. Every run exits 0 with
the same two failures: `Package[haveged]`, and `File[/etc/puppetlabs/puppet]`,
whose `group => puppet` names a group that the filter removed along with its
`Group` resource.

Each default run's report was compared with the `--no-ruby`
and the bad-`--ruby` reports from the same round, using `gatekit
compare-reports` and an allowlist cut down to `run_to_run` alone, with every
`cross_implementation` entry removed. All 10 comparisons found 0 divergent
leaves out of 5,128.

`gatekit run-to-run` on such a pair reports every leaf
as covered by the list. It does report `resource_statuses.*.evaluation_time`
as over-broad, here with 6 stable leaves, which is the shared pattern that
[The SIMP gate](gate-evidence.md#the-simp-gate) already records.

The host
is never launched: `ingestExtensions` returns at
`cmd/voxagent/extension.go` when no type needs it (`len(needed) == 0`), before
`extension.Launch`, which is the only place `--ruby` is used. To show that the
flags really reached voxagent, the full catalog was applied once with each of
them. With `--ruby /nonexistent/ruby` it stopped with "the extension host is
unavailable", and with `--no-ruby` it stopped with "--no-ruby: type(s) …
need the extension host". Both applied nothing.

The walls spread from 11.0 to 20.8 s, and the two slowest were both
`--ruby /nonexistent/ruby` runs. The spread does not come from evaluation.
Every run's native `metrics.time` sum lies between 5.39 and 6.07 s. What
varies is the time from the report's start to the `dnf -y install haveged`
failure, which ran from 4.2 to 13.5 s across the 15 runs. That span holds the
`package` prefetch's `dnf -y check-update`, whose mirror-metadata fetch took
3.98 to 11.09 s when measured alone ([D-57](apply-spine.md)). That flag cannot act
on this catalog, because no code that reads it runs.

### What this shows

On the SIMP catalog, in a container, at a load average of 5 to 10, voxagent
applies through the host in 12% less wall time than Ruby (25.10 against
28.68 s, medians of 5). Most of the saving is before the transaction: 3.90 s
against 5.85 s of start-up and ingest, even though voxagent's figure includes
0.71 s of host start-up. The applied-catalog times differ by 1.6 s, which is
less than either agent's own range (2.7 s for Ruby, 8.3 s for voxagent, whose
range comes from run 1).

The host's Tier 3 evaluation costs about 5% more than
Ruby's own, and `ssh_config` is the only type whose difference is outside the
run-to-run range. On top of that evaluation the host spends 3.35 s, which is
13.7 ms per evaluated resource and an upper bound on what the protocol costs.
The figure held to within 0.3 ms across five runs.

A catalog with no Tier 3
type pays nothing for the host: the host does not start, its flags have no
effect, and the reports agree outside the run-to-run list.

This does not show the cost on a real host, where `Svckill`'s time depends on
the services present and the 62 `Sysctl` that fail here would apply. Nor
does it show the cost on a catalog with more host resources, where
per-resource overhead may not scale linearly, because `load` handles the whole
catalog whatever share of it goes to the host. It does not split the 3.35 s
between `load`, prefetch and framing.

The native comparison is only as good as
its haveged-free line, because the two agents fetch dnf's metadata at
different points ([D-57](apply-spine.md)), and the fetch's 4 to 11 s depends on the mirrors. It
does not show the cost under `--noop`. Every measured run is a first apply on
a fresh container, so a second, converged run was not timed either. Five runs
a side cannot resolve differences smaller than the 0.2 to 0.4 s spread of
most per-type figures.
