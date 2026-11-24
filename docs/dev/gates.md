# Gates

For contributors and maintainers. Back to the [developer docs](README.md).


```bash
task dev:report-diff         # voxagent's report vs Ruby's, identical catalog bytes
task dev:report-diff-extension  # the same for Tier 3 types, run through the extension host
task dev:idempotence         # a second apply must change nothing
task dev:derive-run-to-run   # re-measure report-diff's allowlist against Ruby
task dev:state-diff          # both agents, two containers, same resulting state
task dev:report-diff-container  # the report comparison for package, exec, user, group, concat, the Augeas types, cron, host, sshkey, ssh_authorized_key and mount, in a container
task dev:report-diff-systemd    # the report comparison for service, in a systemd container
task dev:report-diff-simp       # both agents apply the public SIMP catalog, in systemd containers
task dev:augeas-lib             # the libaugeas binding against openvox's real library, in a container
task dev:sensitive-canary       # no Sensitive value leaks from real applies and extension-host routes, in a container
```

`dev:report-diff`, `dev:report-diff-extension` and `dev:idempotence` run inside
`task check`; `dev:report-diff-extension` needs `puppet` and `ruby` on `PATH` and
the vendored stdlib (`task fleet:vendor`). `dev:derive-run-to-run` is not in
`task check`: run it when `dev:report-diff` surfaces a timestamp-shaped divergence,
to tell an incomplete allowlist from a real defect (it depends on `task build`
and compares Ruby Puppet against itself, so it needs `puppet` on `PATH`).
`dev:augeas-lib` is not in `task check` either; it needs podman and builds its
image on first use (see below). `dev:state-diff` and
`dev:report-diff-container` need the fleet node images, built with
`task fleet:images`, and take minutes, so
they are run explicitly, the way `task fleet:gate` is. Run the
gates one at a time: concurrent runs coincided with flakes in the SIMP gate's
polkit start and systemd boot.

`dev:report-diff-container` also runs the Augeas types' half, on an
image derived from the same base, `localhost/fleet-node-el` (`task fleet:images`) (`tests/spine/images/Containerfile.augeas`,
built on first use; a cold build needs the network), and that half, like
`dev:report-diff-simp`, refuses to run under a rootful podman (`podman info --format '{{.Host.Security.Rootless}}'` prints `true` when rootless). It then runs cron's half
(`tests/spine/cron/g5.sh`) on `localhost/cron-measure-el`, the same `fleet-node-el` base plus cronie
(`tests/spine/images/Containerfile.cron`, also built on first use): 21 cases,
each applied live, rerun and under `--noop` by both agents, with the end state of the crontabs compared,
and one catalog both agents must refuse alike.

Then the four ParsedFile types' halves, one after
another on `localhost/parsedfile-measure-el`, an image derived from the same base
(`tests/spine/images/Containerfile.parsedfile-types`, also built on first use): `host`
(`tests/spine/host/g5.sh`), `sshkey` (`tests/spine/sshkey/g5.sh`, once with the vendored sshkeys_core and once
with the SIMP release's), `ssh_authorized_key` (`tests/spine/sak/g5.sh`) and `mount` (`tests/spine/mount/g5.sh`, with the vendored
mount_core and the SIMP release's, in containers given `SYS_ADMIN` so the cases mount real tmpfs file systems).
Each case is applied live, rerun and under `--noop` by both agents, and the end state of the files is compared,
as for cron.

The container report
differential exists because the host is not a valid oracle for `package` or for
`exec`, and for one underlying reason: the host's Ruby cannot fork under Puppet.
So Puppet's `dnf` provider confines itself out and falls back to `dnfmodule`,
which manages dnf *modules*, while every `exec` dies with `Errno::EBADF` before
running anything. Inside the container both work, and it is the only gate that
compares an `exec` event — `executed_command`, `returns` as the property a
non-zero status is attributed to, the refresh callback event — against Ruby.
`user` and `group` are compared there too: they change the machine's
accounts, which only a disposable container may have changed for it.

`dev:sensitive-canary` is run explicitly too: it proves no marked `Sensitive`
value reaches the report, a log line or the console, in real applies of every
type and on the extension-host routes, inside the same image (it needs
`task fleet:images` and `task fleet:vendor`); `go test ./cmd/voxagent -run
TestSensitiveCanarySweep` is its native half.

`dev:report-diff-simp` is the extension host's acceptance gate: it compiles the
public SIMP catalog from pinned forge modules, applies it for real with Ruby
and with voxagent, each in a fresh systemd-booted container, and compares the
reports, the walk order and the end state. It is not part of `task check`: a
cold run needs the network (the forge and the CentOS Stream mirrors) and takes
about five minutes, a warm one about a minute.

Like `dev:report-diff-systemd`,
it boots systemd under rootless podman, which inside a nested container needs a
delegated cgroup subtree. Delegate one once per container boot (the snippet,
and the check that keeps it off a host whose PID 1 is systemd, are in
[Running the systemd gate](../maintainers/gate-evidence.md#running-the-systemd-gate)),
then run the gate from a shell placed in it. The `mkdir` below is only the last step of that
delegation; without the delegation the `echo` to `cgroup.procs` fails:

```bash
mkdir -p /sys/fs/cgroup/$USER/gate
bash -c 'sudo sh -c "echo $$ > /sys/fs/cgroup/$USER/gate/cgroup.procs" && task dev:report-diff-simp'
```

It ends with `report-diff-simp: reports agree outside the allowlist`, exit 0.

See [the extension host notes](../maintainers/extension-host.md) for its
protocol, lifecycle and how to change it, and
[the maintainer notes](../maintainers/apply-spine.md) for the divergence
register, the known defects, and what each gate does and does not prove.
Measured speed and memory against Ruby are in [the performance notes](../maintainers/performance.md).
Puppet behaviour that voxagent reproduces but should eventually change is logged in [the improvements log](../maintainers/improvements.md).
