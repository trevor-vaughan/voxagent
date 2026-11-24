# voxagent

A Go Puppet agent with opportunistic Ruby fallback.

This repository contains **P0**, `voxdiff` and the fleet harness, and **P1a**,
the `voxagent` apply spine.

## voxagent

Applies a Puppet catalog: reads it from a file, refuses anything it cannot fully
implement, applies the rest, and writes a `report_format 12` report.

voxagent is built with cgo (`CGO_ENABLED=1`, Go's default where a C compiler
is installed): `user` and `group` read accounts through libc's name service,
as Puppet does, and `sysctl`, `ssh_config`, `sshd_config`,
`sshd_config_subsystem` and `augeas` load openvox's libaugeas with `dlopen` (the build links
no Augeas library), so a `CGO_ENABLED=0` build fails.

```bash
task build
./bin/voxagent apply --catalog catalog.json --detailed-exitcodes
./bin/voxagent agent --server puppet --detailed-exitcodes   # one run against an OpenVox server
```

`voxagent agent --help` lists its flags; their defaults are Puppet's compiled-in
settings (`puppet.conf` is not read).

### Native units: `voxagent units`

Every Puppet type voxagent applies itself is implemented by a unit. `voxagent units`
prints one JSON array holding every unit's manifest, sorted by name: its version,
the Ruby implementation it reproduces (`source`) and the one it was tested against
(`oracle`), the types it serves, the units it requires, and, for module units, the
file digests of each measured release (a module's type is native only when the
`--libdir` plugin tree matches one of them).

```bash
./bin/voxagent units | jq -r '.[] | "\(.name) \(.version) \(.types | join(","))"'
```

A core unit's element, `jq '.[] | select(.name == "notify")'`:

```json
{
  "name": "notify",
  "version": "1.0.0",
  "source": {
    "implementation": "puppet",
    "version": "8.10.0"
  },
  "oracle": {
    "implementation": "openvox",
    "version": "8.33.0"
  },
  "types": [
    "notify"
  ],
  "requires": [],
  "releases": []
}
```

The command takes no arguments: one prints a usage line on stderr and exits 2.
Otherwise it exits 0. It reads no file, so its output does not depend on the host,
a catalog or `--libdir`.

### Log levels: `--verbose` and `--debug`

Both subcommands take `--verbose` and `--debug`; `--debug` implies `--verbose`,
whichever order they come in. The level applies to the console and to the
report alike. `apply` prints the run's log as `agent` does, in Puppet's console
format and without colour: `Notice: <message>`, or `Notice: <source>: <message>`
for a resource's line. Notice, info and debug lines go to stdout; warnings and
worse go to stderr, and `voxagent: outcome=<status>` stays the last stderr line.

| Flag | Console and report show |
|---|---|
| none | notice and above |
| `--verbose` | also `Info:` lines |
| `--debug` | also `Debug:` lines: Ruby's graph, transaction, `exec`, `file`, `package` and `service` debug lines, and voxagent's own, which name no parameter value, file content, header, certificate, key or HTTP body |

`Sensitive` values are redacted from debug lines as from error text: a marked
command shows as `[redacted]` and its output as `[output redacted]`. A run
without `--debug` writes the same report as before. Debug lines are a subset of
Ruby's (`docs/maintainers/apply-spine.md`, D-82).

Two caps bound a run's memory. A command's output is kept up to 64 MiB, cut at
a line break, or at the last complete UTF-8 sequence when it has none; past it
voxagent logs the warning `<resource>: output of <command> truncated at 64 MiB`
and carries on. The report keeps at most 16 MiB of debug
text; after that it records the warning `debug log truncated in the report
after 16 MiB; the console has the rest` and the console alone shows further
debug lines. Both are fixed, not flags.

Five resource types, each with a bounded attribute set — `notify` (`message`),
`file` (`ensure`, `content`, `mode`, `owner`, `group`, `source`, `recurse`,
`purge`, `path`, `replace`, `force`, `links`, `target`, `show_diff`, `backup`; `mode` is
octal (`0644`) or symbolic (`u+rwX,g-rwx,o-rwx`), computed for each path from
its current mode and whether it is a directory, as Puppet computes it; `source` is
an absolute path or a `file:` URI, or a list of them, `recurse`, `purge`,
`replace` and `force` are `true` or `false`, `links` is `manage` or `follow`,
`ensure` also takes `link` (with `target`) or a path to link to, and
`show_diff` is accepted and has no effect; `backup` is `false`, `true` (the
suffix `.puppet-bak`), a suffix starting with `.`, or `puppet`, the local
clientbucket under `--vardir`; `validate_cmd` checks new content before it
replaces the file, run against a temp file beside it whose path replaces every
`validate_replacement` (default `%`), and is refused with `recurse => true`;
`seluser`, `selrole`, `seltype`,
`selrange` and `selinux_ignore_defaults` manage SELinux contexts, see below),
`exec` (`command`,
`path`, `cwd`, `returns`, `refreshonly`, `creates`, `onlyif`, `unless`,
`logoutput`, `refresh`, `environment`, `umask`, `timeout`, `tries`,
`try_sleep`, `user`, `group`, described below; a `command`, or an `onlyif`
or `unless` check, given as an argv array runs with no shell, a string through `/bin/sh -c`; `logoutput` is `true`,
`false` or `on_failure`, the default, and logs the command's output a line at a
time at notice level, whenever it is `true` and when the exit status is not in
`returns` if it is `on_failure`; output that is not valid in the locale's
encoding, which is US-ASCII unless `LC_ALL`, `LC_CTYPE` or `LANG` names a UTF-8
codeset, fails the exec as Puppet does; locales Ruby resolves differently are
listed in D-79 of `docs/maintainers/apply-spine.md`),
`package` (`ensure`, `name`, `provider`; dnf only), and
`service` (`ensure`, `enable`, `start`, `stop`, `status`, `restart`, `hasstatus`,
`hasrestart`, `name`, `provider`), systemd only; `enable` is `true` or `false`
(`mask`, `manual` and `delayed` are refused).
A catalog with a `service` is refused whole on a host whose PID 1 is not systemd.
Two more, `user` and `group`, are applied natively, through shadow-utils (`useradd`,
`usermod`, `userdel`, `chage`, `chpasswd`, `groupadd`, `groupmod`,
`groupdel`) as Puppet's `useradd` and `groupadd` providers do, only on a
RHEL-family host of major release 10 or later that is not Fedora
(`/etc/os-release` `ID` is `rhel` or `ID_LIKE` lists it) and does not have
libuser's `/usr/sbin/luseradd` and `/usr/sbin/lgroupadd` both installed. Each type is decided per
run, on its own: it stays with the Ruby extension host when any of its
resources names a `provider` other than `useradd` or `groupadd`, and `user`
does when any `user` sets `purge_ssh_keys` to anything but `false`, `"false"` or `[]`.
Every attribute either type declares is accepted, and those its provider
does not support (`forcelocal`, and the Solaris, AIX, BSD and macOS ones) are ignored, as
Puppet ignores them. Everywhere else `user` and `group` need the extension
host, and `--no-ruby` refuses a catalog that has them.
`concat_file` and `concat_fragment` (puppetlabs-concat) are applied natively
when the synced module under `--libdir` is release 9.1.0 or 10.0.1 (voxagent
identifies it by the digests of its two type files) and every one of them in the
catalog uses a supported shape: `format` `plain` (the default), no fragment
`source`, `backup` not `true`, and clean, distinct absolute paths. Otherwise
both types stay with the Ruby extension host, and `--no-ruby` refuses a catalog
that has them. A fragment `order => 2.0` is accepted natively as `2`, but
refused as Ruby does when the host is loaded for another type in the same
run.
`sysctl`, `ssh_config`, `sshd_config` and `sshd_config_subsystem`
(augeasproviders_sysctl and augeasproviders_ssh) are applied natively, through
the libaugeas 1.15.0 that openvox bundles
(`/opt/puppetlabs/puppet/lib/libaugeas.so.0`), when all of these hold: openvox
8.33.0 is installed with that library; the synced modules under `--libdir` are
augeasproviders_core 4.2.0 with augeasproviders_sysctl 4.1.0 and
augeasproviders_ssh 7.0.0 (voxagent identifies them by the digests of their
type and provider files); none of `AUGEAS_ROOT`, `AUGEAS_LENS_LIB`,
`AUGEAS_DEBUG`, `AUGEAS_DEBUG_DIR`, `LD_PRELOAD` and `LD_LIBRARY_PATH` is set
in voxagent's environment; `--libdir` is an absolute, clean path; and every
resource of the type stays inside the supported shapes (no `comment`, no
`Sensitive` value, and for `sysctl` no `val`, `persist => false` or `ensure =>
absent`; the names, hosts, conditions and targets are plain strings of the
usual characters). `sysctl` also needs a RHEL-family host of major release 10
or later that is not Fedora, with `sysctl` on `PATH`, `/usr/sbin` or `/sbin`.
Each type is decided on its own; a type that is not native is applied by the
Ruby extension host, which is then required, and `--no-ruby` refuses a catalog
that has one. `--debug` logs why a type was delegated. Custom lenses in
`<libdir>/augeas/lenses` are used as Puppet uses them.
The generic `augeas` type (augeas_core 2.1.0) is applied natively under the
same library, environment and `--libdir` conditions, when `--libdir` holds
augeas_core 2.1.0 (identified by the digests of its type, provider and parser
files; augeasproviders_core is not needed) and every `augeas` resource has a
supported shape: `changes` of `set`, `rm`/`remove`, `clear`, `touch`,
`ins`/`insert`, `defvar`, `defnode` or `mv`/`move`, at least one command, all of
printable ASCII, `onlyif` unset or `''`, `force` unset or `false`, `root` unset
or `/`, `load_path` unset or `''`, `type_check` unset, `false` or `'false'`,
`returns` unset or `0`, `show_diff` a valid boolean, `provider` unset or
`augeas`, and `lens` and `incl` given together or not at all. Anything else (`onlyif`, `force`, `setm`,
`clearm`, `rename`, a `changes` that does not parse, non-ASCII text) sends the
type to the Ruby extension host, and `--no-ruby` refuses it. `augeas` is decided
apart from the other four. It never prints a diff (`show_diff` has no effect).
`voxagent agent` loads `<libdir>/augeas/lenses` for `augeas` as Puppet's agent
does; `voxagent apply` does not, and does not read modulepath lenses. The
extension host never sets a run mode (Puppet defaults to `user`), so an `augeas`
resource it applies gets lens path `""` under both commands. Two changes follow:
an `agent` run whose libdir ships lenses (SIMP's `resolv.aug`) now uses them,
which voxagent did not before this slice, and the lens path changes when the
type is routed to the host (for example by one resource with `onlyif`).
Of the metaparameters, `loglevel` is accepted on any resource only as `notice`,
Puppet's default, which `concat` copies onto every file it generates.
Any other type is applied by the Ruby extension host if it is a legacy
`Puppet::Type.newtype` type and the host can run (below). Anything else is
refused **before** anything is applied, so a run either applies
the whole catalog or changes nothing. Declaring an attribute is a promise to
read it, so the refusal list is the honest statement of coverage — and doubles
as the porting roadmap.
Some catalogs Puppet itself would refuse are refused the same way, after the
coverage check and before anything is applied, and reported in Puppet's own
words. Today: a `file` whose path is not absolute, whose `source` is relative,
opaque or unparseable, or that sets both `content` and `source`; and an `exec`
with a relative `cwd` or `creates`, or an unqualified command and no `path`.
voxagent also refuses, before anything is applied, a `file` whose `source`
names a host, a `file` with `content` and `recurse => true`, a `file` with
`links => follow` and `recurse => true`, and a static catalog carrying inlined
file metadata.

`exec` runs each command, each `onlyif` or `unless` check and the `refresh`
command as Puppet does:

- `timeout` is in seconds, default 300, and `0` means none. Each run of a
  command gets the whole budget: every try, every check, the `refresh`
  command. On expiry the command's process group gets SIGTERM once, with no
  escalation, and the exec fails with `Command exceeded timeout`. A command
  that traps TERM keeps running. Only cancelling the run while a command is
  still running sends its process group SIGKILL (D-83 in
  `docs/maintainers/apply-spine.md`).
- `tries` (default 1) runs the command until its exit status is in `returns`,
  sleeping `try_sleep` seconds (default 0) after each failed try, the last one
  included, as Puppet does. Checks and the `refresh` command are not retried.
- `environment` is a `NAME=value` string or a list of them, set after `path`.
  Puppet's three warnings are reproduced: an element it cannot parse, a
  variable set twice (or `PATH` while `path` is set), and an empty value
  (once per variable per run, as Puppet does).
  Every command voxagent starts, including `service`'s and `package`'s, runs
  without `HOME`, `USER` and `LOGNAME`, as under Puppet.
- `umask` is an octal string (`'0027'`) for the command and its checks.
- `user` and `group` are names or numeric ids, and need the agent to run as
  root (a non-root agent may name only its own user). `cwd` is entered as
  root first, so a `user` command can start in a directory that user cannot
  enter. No `HOME` is set for the user. `user` gives the command the user's
  groups and its primary group, or `group`'s; `group` alone changes the
  primary group and keeps the agent's supplementary groups. An unknown user or
  group fails the exec without starting the command.
- `refresh` is run instead of `command` when the exec is refreshed: once, with
  no `tries` and no `logoutput`, after the same checks. Its exit status is
  ignored, so a `refresh` command that exits 1 is reported as a successful
  refresh. Only exit 127 (command not found) fails it. This is Puppet's rule,
  logged as I-31 in `docs/maintainers/improvements.md`.
- Exit status 127 from any of these commands fails the exec with the
  command's output as the message, whatever `returns` says.

A `Sensitive` value is redacted as Puppet redacts it. The properties it marks
show `[redacted]` in the report and the log, and an `exec` with a marked
`command`, `unless` or `onlyif` shows `[command redacted]` in its error text
and noop message and logs `[output redacted]` for its output. voxagent also
keeps a marked value out of the error text where Puppet prints it. A parameter that is not a property (`cwd`, `environment`) cannot
be redacted automatically: voxagent warns on the console, as Puppet does,
`voxagent: Warning: /Exec[x]: Unable to mark 'cwd' as sensitive: …` under `apply`
and `Warning: /Exec[x]: …` under `agent`, and leaves the warning out of the
report. The value of such a parameter is not hidden, so a command that echoes a
marked `environment` value prints it. The file voxagent writes holds the
cleartext, and voxagent shows no diffs. A `file` recursion child inherits owner,
group and mode from a parent whose values are marked but carries no marks, as in
Puppet, so its events show those values in the clear.

Rich catalog values (`__ptype` in the catalog JSON) are decoded as
`puppet apply` decodes them. An ASCII `Binary`, which openvox's compiler emits
for Facter's networking facts, becomes its string and applies on any resource.
Every other rich value (a `Binary` that is not ASCII, `Timestamp`, `Regexp`,
`SemVer`, `URI`, a `Hash` with non-string keys, `Error`, a `Pcore` type, a
`Sensitive` map) applies only on a resource the Ruby extension host applies
(below), which hands it to the type's own code as Ruby does. On a native
resource it is refused before anything is applied, one line per parameter:

```
voxagent: <Ref>: parameter <name> holds a Binary value that is not ASCII, which voxagent does not support on a native resource
voxagent: <Ref>: parameter <name> holds a rich <Type> value, which voxagent does not support
```

A `Deferred` value is refused on every resource that is not a container,
before the host starts (Ruby never resolves one on a container either):
`voxagent: <Ref>: parameter <name> is Deferred; voxagent does not resolve
Deferred values`, then `voxagent: <n> Deferred value(s); nothing applied`.
An unknown `__ptype` name, or a `Binary` that is not valid base64, fails the
whole catalog with Puppet's own message.

Resources are applied in catalog order, the order the compiler evaluated the
manifest in, except where a relationship (`before`, `require`, `notify`,
`subscribe`, a chaining arrow, or a type's autorequire) says otherwise: like
Puppet, voxagent always applies, among the resources whose dependencies are
done, the one that comes first in the catalog. A class or defined type is
ordered as a whole, as Puppet orders it, and a resource a type generates takes
the place Puppet gives it. A resource the extension host applies whose type
has no suitable provider on this host is set aside until nothing else is
ready, offered again, and failed if it is still unsuitable, as Puppet does.

A symlink at a managed `file` path is not refused: under `links => manage`,
Puppet's default, `ensure => file` or `directory` replaces it, and under
`links => follow` the link's target is what is managed. A directory in the way
is replaced only with `force => true`; without it voxagent logs Puppet's
notice and the resource fails or, for `ensure => absent`, reports `removed`
while the directory stays, as Puppet does.

SELinux contexts are managed only on a host where SELinux is enabled (a
read-write selinuxfs and `/etc/selinux/config`); elsewhere the `sel*`
attributes do nothing. A `file` gets the policy's default context unless
`selinux_ignore_defaults => true`, and a declared `seluser`, `selrole`,
`seltype` or `selrange` replaces that field. The defaults come from
`selabel_lookup`, so an SELinux-enabled host needs the `libselinux-utils`
package: without it a catalog with a `file` that needs a default (not a
removal, and not one declaring all four fields) is refused whole before
anything is applied. So is such a catalog when the policy's file contexts
cannot be opened (`Cannot get default context with nil handle`), as Puppet
fails its whole run then.
Each default is one `selabel_lookup` process, 4 ms or more; a recursion looks
its children's defaults up in batches through `matchpathcon` instead.

### File backups and validation

`backup` defaults to `false`, as in Puppet 8. A backup is taken just before a
`file` replaces or removes what is at its path, or rewrites an existing file's
content; never for a new file or a mode-only change, and never under `--noop`.
A backup that cannot be made fails that change and leaves the path as it was.
The exception is a path Puppet cannot back up at all: a fifo, socket or device
there only gets the warning `Could not back up file of type <type>` and is then
replaced without a backup, as Puppet does.

- **A suffix** (`true` is `.puppet-bak`) copies the path to `<path><suffix>`,
  keeping mode, owner and times; a directory is copied whole, but only with
  `force => true`. An old copy that is a file is replaced. One that is a
  directory refuses the resource on every run with `Will not remove directory
  backup <path><suffix>; use a filebucket`, as Puppet does, until you remove it.
- **On a filesystem where root does not own the directories it makes** (NFS
  with `root_squash`), a suffix backup of a directory is refused: voxagent
  cannot tell the directory it made from one swapped in by someone else, so it
  writes nothing into it and leaves it, empty and `0700`. Later runs then meet
  the refusal above. Remove it with `rmdir '<path><suffix>'` (which only removes
  an empty directory), and give such a resource `backup => false` or
  `backup => 'puppet'`.
- **`puppet`** stores the old content in the local clientbucket,
  `<vardir>/clientbucket`, byte for byte as Puppet writes it (the gates compare
  the two). Set the vardir with `--vardir` (default
  `/opt/puppetlabs/puppet/cache`, Puppet's); it is created if missing. A
  directory's files are bucketed one by one. A symlink anywhere under the
  clientbucket fails the resource instead of being followed (Puppet follows
  it). Any other bucket name fails the resource with `Could not find
  filebucket <name> specified in backup`. A catalog that declares a
  `filebucket` resource is refused before anything is applied (exit 2), even
  one named `puppet`: `file`'s backup implements only the default bucket, so
  it could not use the declared one.

`validate_cmd` runs before new content replaces a file, against a temp file
beside it, and the file is replaced only when the command exits 0. Every
`validate_replacement` (default `%`) in the command becomes the temp file's
path; without one the command runs with no file argument and validates
nothing, as in Puppet. The command runs as the user running voxagent, in its
working directory, with no timeout, as Puppet runs it; nothing runs under
`--noop`. Its environment drops `HOME`, `USER`, `LOGNAME` and the locale
variables and sets `LANG=C` and `LC_ALL=C`. Its `PATH` is voxagent's own:
Puppet appends any missing `/usr/sbin` and `/sbin`, voxagent does not, so name
the validator by its full path.

### Ruby extension host

A catalog may hold types voxagent does not apply natively on this run: `User` and
`Group` off the platform they were measured on, module types whose plugin tree
it does not recognise, other types that modules ship, `file_line` from stdlib. If they are legacy
`Puppet::Type.newtype` types, voxagent starts one Ruby process, the extension
host, which loads them and evaluates each with Puppet's own code. voxagent keeps
the ordering, refresh propagation and the report. A catalog with only native
types never starts Ruby.

```bash
./bin/voxagent apply --catalog catalog.json \
  --ruby /opt/puppetlabs/puppet/bin/ruby \
  --libdir /opt/puppetlabs/puppet/cache/lib
```

| Flag | Default | Meaning |
|---|---|---|
| `--ruby <path>` | `/opt/puppetlabs/puppet/bin/ruby` | Interpreter for the host. Must be an absolute path and able to load the `puppet` gem. It is never looked up on `PATH` |
| `--libdir <dir>` | `/opt/puppetlabs/puppet/cache/lib` | The pluginsync directory the host loads types from. voxagent does not read `puppet.conf` |
| `--vardir <dir>` | `/opt/puppetlabs/puppet/cache` | Puppet's vardir, shared with the host. `backup => 'puppet'` keeps the old content in `<dir>/clientbucket`, as Puppet's `clientbucketdir` does, and Tier 3 types keep their own state there (below). The default is root's openvox cache, so a run as another user that backs up to `puppet` (every `File` concat generates does), or that applies Tier 3 types, needs `--vardir` set to a directory it can write |
| `--no-ruby` | off | Refuse any catalog that needs the host, naming the types. No interpreter is started |
| `--extension-timeout <d>` | `5m` | Limit for each host request other than evaluating a resource |
| `--extension-evaluate-timeout <d>` | `1h` | Limit for each resource's evaluation, which includes the provider's prefetch |

Durations are Go durations (`90s`, `5m`) and must be positive.

What to expect:

- stderr names each type the host will apply, with its resource count and where
  the type came from (`core` or `libdir`), and, after the last resource, prints one
  line giving the host's start-up time, the resources it evaluated and the time
  spent in it. The `outcome=` line stays the last line on stderr.
- The host shares `--vardir`, as an agent run does, so Tier 3 types write
  their own state there, such as simplib's `reboot_notify` and its
  `reboot_notifications.json`. A non-root run with Tier 3 types therefore needs
  a writable vardir. voxagent never writes Puppet's `state.yaml` there.
- The host runs with only `PATH`, `HOME`, `LANG`, `LC_ALL`, `GEM_HOME` and
  `GEM_PATH` from your environment. Module code runs as the user running
  voxagent, with the same trust Puppet gives pluginsynced code. The host's own
  stdin is `/dev/null`, and what module code prints goes to stderr.
- Puppet's own log lines for those resources are in the report. A host that
  fails or does not answer in time is killed, not restarted: the Tier 3
  resources not yet evaluated fail, their dependents are skipped, native
  resources keep applying, and the run reports failures.
- Resources a type generates are applied. Generation done before the run
  (`generate`) happens when the host loads the catalog; generation during the
  run (`eval_generate`, which `concat` uses) happens at the generating
  resource's turn. Either way Puppet's own code generates them, and voxagent
  applies each new resource through its own type, natively when it is one of
  the native ones. A resource `eval_generate` makes that voxagent cannot apply
  fails, and the generating resource's dependents are skipped, rather than
  being half-applied; one generated at load refuses the run (below).
- A Tier 3 resource can be referred to by its name as well as its title, as in
  Puppet.
- Refused before anything is applied, with could-not-run: `--no-ruby` on a
  catalog that needs the host, a host that cannot start (no Ruby, or one that
  cannot load Puppet), a resource Puppet itself would refuse (every one is
  listed, where Puppet stops at the first), a Resource API type, an unknown
  type, and a resource generated at load that voxagent cannot apply.

Exit codes follow Puppet's, measured rather than assumed. Plain `puppet apply`
exits 0 for changes *and* for failures — its status says only that the tool ran
— so voxagent does the same, and `--detailed-exitcodes` carries the verdict as
Puppet's 0/2/4/6 bitmask. "Could not run" takes the value each mode leaves free:
`2` plain, `1` under the flag. Every run prints `outcome=` to stderr so the
status never has to be decoded from the argv.

One case differs from Puppet on purpose. A relationship naming a Tier 3
resource by a title Puppet cannot look up, because the resource's type
rewrites its title (`Kernel_parameter[fips]`, which Puppet names
`Kernel_parameter[fips:all]`), is refused before anything is applied, as
could not run (`2`, or `1` under `--detailed-exitcodes`):
`voxagent: Failed to apply catalog: Could not retrieve dependency '<ref>' of
<ref>; nothing applied`. Puppet prints the same error, applies nothing,
writes a failed report and exits `0`, even under `--detailed-exitcodes`.

### Running against an OpenVox server

`voxagent agent` is one Puppet agent run: it enrols with the server's CA,
fetches what the catalog needs, applies the catalog with the same spine as
`voxagent apply`, and reports back. There is no daemon; run it from a timer
(below).

```bash
sudo ./bin/voxagent agent --server puppet.example.com --detailed-exitcodes
```

One run does, in Puppet's order:

1. **SSL bootstrap** under `--ssldir`: downloads the CA, the CRL and, on a
   first run, submits a CSR for `--certname` and fetches the signed
   certificate. A steady-state run makes no certificate request.
2. **Node request**: the server's classification may override
   `--environment`.
3. **Pluginsync** of the `plugins` and `pluginfacts` mounts into `--libdir`
   and `--pluginfactdest`.
4. **Facts** from `facter`, sent with the catalog request.
5. **Catalog request**, then every `puppet:///` source in it is fetched
   before anything is applied.
6. **Apply**, through the Go spine and, for Tier 3 types, the Ruby extension
   host.
7. **Report**: `PUT` of the run's report to the server.

Run it as root, on a node with `openvox-agent` installed: `facter` and the
Ruby the extension host loads come from that package, at the default paths in
the table below.

| Flag | Default | Meaning |
|---|---|---|
| `--server` | `puppet` | Server host name. The server certificate must name it. |
| `--serverport` | `8140` | Server port. |
| `--certname` | the FQDN, lower case | This node's certificate name. |
| `--environment` | `production` | Environment to request; the server's node classification may override it. |
| `--ssldir` | `/etc/puppetlabs/puppet/ssl` | CA bundle, CRL, key and certificate. |
| `--ca-fingerprint` | unset | Pin the CA; see below. |
| `--waitforcert` | `0` | Poll interval while the certificate is unsigned, in Puppet's syntax: seconds (`120`) or a number with `s`, `m`, `h`, `d` or `y` (`30s`, `2m`). `0` exits instead of waiting. Puppet's default is `2m`. |
| `--facter` | `/opt/puppetlabs/puppet/bin/facter` | Absolute path of the facter binary. |
| `--libdir` | `/opt/puppetlabs/puppet/cache/lib` | Pluginsync target; the extension host loads types from here. |
| `--pluginfactdest` | `/opt/puppetlabs/puppet/cache/facts.d` | Pluginsync target for external facts. |
| `--vardir` | `/opt/puppetlabs/puppet/cache` | Holds the run's downloaded `puppet:` sources and the clientbucket. |
| `--ruby` | `/opt/puppetlabs/puppet/bin/ruby` | Ruby for the extension host. |
| `--no-ruby` | off | Refuse catalogs that need the extension host. |
| `--extension-timeout` | `5m` | Limit per extension host request other than evaluate. |
| `--extension-evaluate-timeout` | `1h` | Limit per extension host evaluate request. |
| `--http-timeout` | `2m` | Limit per HTTP request, including reading the response. |
| `--noop` | off | Report what would change without changing it. |
| `--detailed-exitcodes` | off | Puppet's exit bitmask. |
| `--verbose` | off | Also log info-level lines, to the console and the report. |
| `--debug` | off | Also log debug-level lines; implies `--verbose`. See "Log levels". |
| `--report-dir` | unset | Also write the run's report to this directory. |

Defaults are Puppet's compiled-in settings; `puppet.conf` is not read.
`voxagent agent --help` is the authority on the flags.

**Console output.** The run's log lines are printed as Puppet's console prints
them, without colour: `Notice: <message>`, or `Notice: <source>: <message>`
for a resource's line. Notice and info lines go to stdout; warnings and worse
(`Warning:`, `Error:`, ...) go to stderr. The level is Puppet's default,
notice; `--verbose` lowers it to info, for the console and the submitted
report alike, so `--verbose --detailed-exitcodes` is the equivalent of
`puppet agent -t`; `--debug` lowers it to debug. voxagent's own progress and
error lines go to stderr prefixed `voxagent: `, ending with the
`voxagent: outcome=<status>` line.

**Trust on first use.** Like Puppet, voxagent downloads the CA certificate on
first contact without being able to verify it, and logs its fingerprint. A
network attacker present at that moment could substitute their own CA, so pass
`--ca-fingerprint` on the first run (and keep passing it): the SHA-256 of the
root CA certificate's DER, in hex, colons optional.

```bash
openssl x509 -in ca.pem -outform DER | sha256sum
```

`openssl x509` reads the first certificate in the file; if the CA has an
intermediate, use the self-signed root.

This is **not** Puppet's `ca_fingerprint` setting, which digests the PEM of
the whole bundle; a value copied from Puppet will mismatch. A mismatch saves
nothing and the run exits could-not-run. The pin is also checked every time
`ca.pem` is loaded and on every CA refresh. After the first run, every request
to the server is verified against the saved CA, the server's name, and the CRL,
except the CA and CRL refreshes, which (as Puppet) do not check revocation.

Because the agent runs as root and loads pluginsynced Ruby, it refuses to run
if `--libdir`, `--pluginfactdest`, `--vardir`, the facter binary, or any
parent directory of them is writable by anyone but root or the running user,
or if the key, `ca.pem` or `crl.pem` is owned by someone else or writable by
group or world, or if the private key is world-readable. Pluginsync fails closed: any error stops the run before
facter or the extension host loads anything.

Exit codes are those of `voxagent apply`, described above. Any failure before
the catalog is applied (network, TLS, pluginsync, a source that cannot be
fetched, a compile error) is could-not-run, nothing is applied, and no report
is sent. A report that cannot be sent is logged and does not change the exit
code.

Scheduling: with systemd, a oneshot service and a timer; with cron, a line.

```ini
# /etc/systemd/system/voxagent.service
[Service]
Type=oneshot
ExecStart=/usr/local/bin/voxagent agent --server puppet.example.com --ca-fingerprint <sha256> --detailed-exitcodes
SuccessExitStatus=2

# /etc/systemd/system/voxagent.timer
[Timer]
OnBootSec=2min
OnUnitActiveSec=30min
RandomizedDelaySec=5min
[Install]
WantedBy=timers.target
```

`SuccessExitStatus=2` is because `--detailed-exitcodes` returns 2 for "changes
applied". `RandomizedDelaySec` stands in for Puppet's splay. Two runs at once
are not prevented (there is no run lock); a oneshot unit cannot overlap itself.

Not implemented: daemon mode, splay, a run lock, cached catalogs (a down
server fails the run), `puppet.conf`, static catalogs, HTTP proxies
(proxy environment variables are ignored), `server_list` failover, certificate
renewal, CA rotation, `csr_attributes.yaml`, `last_run_summary.yaml`, and
SPIFFE or catalog signing. The deliberate differences from `puppet agent` are
registered as D-59 to D-77 in
[`docs/maintainers/apply-spine.md`](docs/maintainers/apply-spine.md); how it is
built is in [`docs/maintainers/agent-transport.md`](docs/maintainers/agent-transport.md).

### Gates

```bash
task dev:report-diff         # voxagent's report vs Ruby's, identical catalog bytes
task dev:report-diff-extension  # the same for Tier 3 types, run through the extension host
task dev:idempotence         # a second apply must change nothing
task dev:derive-run-to-run   # re-measure report-diff's allowlist against Ruby
task dev:state-diff          # both agents, two containers, same resulting state
task dev:report-diff-container  # the report comparison for package, exec, user and group, in a container
task dev:report-diff-systemd    # the report comparison for service, in a systemd container
task dev:report-diff-simp       # both agents apply the public SIMP catalog, in systemd containers
task dev:augeas-lib             # the libaugeas binding against openvox's real library, in a container
task dev:sensitive-canary       # no Sensitive value leaks from real applies and extension-host routes, in a container
```

`dev:report-diff`, `dev:report-diff-extension` and `dev:idempotence` run inside
`task check`; `dev:report-diff-extension` needs `puppet` and `ruby` on `PATH` and
the vendored stdlib (`task fleet:vendor`). `dev:state-diff` and
`dev:report-diff-container` need a built container image and take minutes, so
they are run explicitly, the way `task fleet:gate` is. Run the
gates one at a time: concurrent runs coincided with flakes in the SIMP gate's
polkit start and systemd boot. `dev:report-diff-container` also runs the Augeas types' half, on an
image derived from the same base (`tests/spine/images/Containerfile.augeas`,
built on first use; a cold build needs the network), and that half, like
`dev:report-diff-simp`, refuses to run under a rootful podman. The container report
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
about five minutes, a warm one about a minute. Like `dev:report-diff-systemd`,
it boots systemd under rootless podman, which inside a nested container needs a
delegated cgroup subtree. Delegate one once per container boot (the snippet,
and the check that keeps it off a host whose PID 1 is systemd, are in
[Running the systemd gate](docs/maintainers/apply-spine.md#running-the-systemd-gate)),
then run the gate from a shell placed in it:

```bash
mkdir -p /sys/fs/cgroup/$USER/gate
bash -c 'sudo sh -c "echo $$ > /sys/fs/cgroup/$USER/gate/cgroup.procs" && task dev:report-diff-simp'
```

It ends with `report-diff-simp: reports agree outside the allowlist`, exit 0.

See [the extension host notes](docs/maintainers/extension-host.md) for its
protocol, lifecycle and how to change it, and
[the maintainer notes](docs/maintainers/apply-spine.md) for the divergence
register, the known defects, and what each gate does and does not prove.
Measured speed and memory against Ruby are in [the performance notes](docs/maintainers/performance.md).
Puppet behaviour that voxagent reproduces but should eventually change is logged in [the improvements log](docs/maintainers/improvements.md).

## voxdiff

Compiles a Puppet catalog twice and reports any divergence, ignoring the two
fields that legitimately vary between compiles.

### Build

```bash
task build
```

### Compare two compiles

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

### Compare two implementations

`-left` and `-right` select the binary for each side, so the same tool that
verifies Ruby against itself today will verify Go against Ruby later:

```bash
./bin/voxdiff compare -node web01 -left puppet -right /usr/bin/openvox
```

### Scan for non-deterministic constructs

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

## Fleet harness

A containerised Puppet fleet — layered hiera, three nodes with different facts, real SIMP
modules, and real exported resources through OpenVoxDB — used to prove the allowlist holds
against realistic data rather than a flat fixture.

```bash
task fleet:up      # clone pinned sources, build images, start, wait for OpenVoxDB
task fleet:gate    # run the whole gate (brings the stack up first if it is not)
task fleet:down    # stop, keeping volumes
task fleet:clean   # stop and drop volumes
```

The gate is a Venom suite, `fleet/tests/fleet.venom.yml`, with eight checks: the per-node and
per-OS-family hiera axes are live, every node compiles identically twice, that determinism
check can be made to go red and recovers, the cross-implementation comparison completes, the
module-tree scan is not truncated, exported resources diverge as expected,
`compliance_markup` enforcement reaches the catalog, and that compliance check can be made
to go red and recovers.

Each check is also a task of its own — `fleet:gate:axes`, `fleet:gate:determinism`,
`fleet:gate:negative`, `fleet:gate:implementations`, `fleet:gate:scan`, `fleet:gate:exports`,
`fleet:gate:compliance`, `fleet:gate:compliance-negative`.
Run them with `-x`, because `task` reports `201` for any non-zero exit and these scripts
distinguish `1` ("a result about the fleet") from `2` ("could not run"):

```bash
task -x fleet:gate:axes
```

Two of those tasks mutate the fixture the containers are compiling. `fleet:gate:negative`
briefly rewrites `fleet_profile`'s `init.pp`; `fleet:gate:compliance-negative` briefly
renames `fleet_profile`'s `SIMP/` directory away. Both are bind-mounted into all three
containers, so run either one alone — never alongside another gate task or a second
terminal, which would silently measure the broken fixture. Both restore what they touched,
on their normal path and from an `EXIT` trap; `task fleet:gate` is safe because Venom runs a
suite's testcases sequentially.

Requires `podman` and `podman-compose`. Artifacts are written to `.test-output/fleet/`.
See [the maintainer notes](docs/maintainers/fleet.md) for the environment constraints that
are not obvious from the compose file, and for what the gate does and does not prove.

## Testing

```bash
task test              # human-readable output
task test MODE=llm     # failures only
task integration:venom # end-to-end against a real Puppet
task check             # all quality gates
```

Artifacts are written to `.test-output/`.

## License

Apache License 2.0; see [LICENSE.md](LICENSE.md). voxagent is a derivative
work of [Puppet](https://github.com/puppetlabs/puppet), which is licensed the
same way; the attribution is in [NOTICE](NOTICE).
