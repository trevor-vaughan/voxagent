# Maintainer notes: what the spine does not implement

For maintainers. Split out of the [apply spine](apply-spine.md) notes, which define the terms used here (G1 to G5, Tier 3) and hold the divergence register the D-rows below refer to. Back to the [README](../../README.md).

`file` is bounded to `ensure`, `content`, `mode`, `owner`, `group`, `source`
(local paths and `file:` URIs), `recurse` and `purge` (both `true`/`false`),
`path`, `replace`, `force`, `links` (`manage` or `follow`), `target` and
`show_diff` (accepted, no effect), the SELinux attributes `seluser`, `selrole`,
`seltype`, `selrange` and `selinux_ignore_defaults`, and `backup` (a suffix, or
`puppet`, the default bucket under `--vardir`; any other bucket name fails as
Puppet's missing bucket does, and a catalog that declares any `filebucket`
resource is refused by the pre-flight check, D-50), and `validate_cmd` with
`validate_replacement` (strings only, and not with `recurse => true`;
`internal/units/file/file_validate.go`, D-48).

Of the metaparameters, `loglevel` is accepted on any type only as `notice`,
Puppet's default, which `concat` copies onto every `File` it generates; any
other level is refused like any unimplemented attribute (`internal/ral/check.go`).

A `Concat_file` that `require`s or `subscribe`s `File[<path>]` when the
catalog's `File` at that path is titled otherwise gets a dependency cycle, in
Ruby and in voxagent alike: the generated `before` is skipped only for a
relationship naming the `File` by its own ref, compared as written
(`additional_resource_generator.rb:182-191`). Unmeasured.

`exec` is bounded to `command`, `path`, `user`, `group`, `cwd`, `logoutput`,
`refresh`, `environment`, `umask`, `timeout`, `tries`, `try_sleep`, `returns`,
`refreshonly`, `creates`, `onlyif`, `unless`. `notify` is bounded to `message`. `package` is bounded to
`ensure`, `name` and `provider` — three of the twenty-three attributes the type
declares, with `provider` recognised **only** so that a value other than `dnf`
can be refused. `service` is bounded to `ensure`, `enable`, `hasstatus`,
`hasrestart`, `start`, `stop`, `status`, `restart`, `name` and `provider`.

Everything else — and every resource type other than `notify`, `file`, `exec`,
`package` and `service`, and `user`, `group`, `concat_file`, `concat_fragment`, `sysctl`, `ssh_config`, `sshd_config`, `sshd_config_subsystem`, `augeas`, `cron`, `host`, `mount`, `sshkey` and `ssh_authorized_key` where they are native (below, and D-91) —
is refused by the pre-flight check or handed to the extension host, not silently omitted,
and the refusal covers the **whole catalog**: a partially applied node produces
state-diff divergences that are artifacts of coverage rather than defects.

`file` and `package` bound an attribute's **values**: `file` bounds `source`,
`recurse` and `purge`, `package` bounds `ensure` and `provider`.
Pre-flight hands a non-string value to the bound as its string form, and splits
a list into elements only for an attribute the provider declares list-valued
(`ral.ListValued`) -- `file`'s `source` alone -- so a list anywhere else is
still judged whole and refused.

For `package`, `ensure` is `present`, its alias `installed`, `absent`, `latest`, or a concrete
version string; `purged`, `disabled`, `held` and every version range are
refused (D-13). This is why `Gap` carries a `Value` and pre-flight grew a third
message form, `Package[foo]: ensure => "purged" is not implemented`: `file`'s
`ensure` has five keywords (`present`, `absent`, `file`, `directory`, `link`; any other string is a link target) and catching a bad one at apply time is nearly harmless,
where `package`'s carries an open-ended version string and an apply-time refusal
would apply the rest of the catalog before noticing. `file`'s apply-time
`default:` branch is deliberately unchanged: if we changed a shipped provider
when no consumer asked for it, we would alter its behaviour and nobody requested
that.

## `service`

`service` is the systemd provider and nothing else. `provider` is recognised
only so another value can be refused; `enable => mask` is refused at pre-flight,
and `manual` and `delayed` fail Puppet's own feature check for systemd.
`hasstatus` and `hasrestart` are accepted and have no effect, because the
systemd provider always has a status and a restart command (`systemd.rb:188,201`).

- **The enable change rides inside ensure's.** When `ensure` changes and `enable`
  is out of sync, `enable` is synced inside the `ensure` change and gets no event
  of its own (`type/service.rb:120-136`, `resource_harness.rb:84-99`); under noop
  it is not reported at all. When `ensure` is in sync or unmanaged, `enable` is
  its own change.
- **`InvalidateRefreshes`.** A change that starts the service sets
  `Change.InvalidateRefreshes`, which drops the resource's own pending refresh:
  the start event carries `invalidate_refreshes` (`type/service.rb:109`), so a
  service started this run is not then restarted by its notifiers
  (`event_manager.rb:82`).
- **The init check is `Validate`'s, not `Prefetch`'s.** A host whose PID 1 is not
  systemd refuses the whole catalog (D-22), because a prefetch failure would let
  every other type apply first.
- **Custom commands.** A `start`, `stop`, `status` or `restart` given as a string,
  a single-element array or a scalar runs through `/bin/sh -c`, as Puppet's does
  (`util/execution.rb:360,391`): the catalog author's shell, as `exec`'s command
  is. A multi-element array runs as direct argv. Every built-in command is argv
  with `--` before the unit name, never a shell.
- **Failures carry the journal.** A failed start, stop or restart appends
  `journalctl`'s last lines, run as argv under `env LANG=C LC_ALL=C`. See D-18,
  D-19 and D-21 for where this differs from Puppet.

D-10, D-18, D-19, D-20, D-21 and D-22 are `service`'s; D-24 is shared with `exec`; D-23, also shared with `exec`, is retired. `task dev:report-diff-systemd`
compares voxagent's report with Ruby's in a container with systemd as PID 1.

Declaring an attribute is a promise to read it. `withpath` was removed from
`notify` for exactly this reason rather than implemented: pre-flight accepted a
catalog setting it, the run reported success, and the attribute had no effect.

The refusal list doubles as the porting roadmap: run `voxagent apply` against a
fleet's catalogs and the gaps it prints, ranked by frequency, are the measured
answer to "what should Tier 1 cover next". With the extension host, the same
ranking is the `N <type> resource(s) applied via extension host` lines. On the
SIMP gate's catalog (737 resources), measured 2026-11-19 before `user` and
`group` went native, 246 resources went to the host or would have been
refused without it: 210 of module types, which voxagent can only delegate, and
36 of core types applied through the host (`augeas` 18, `mount` 5, `cron` 4,
`group` 4, `user` 3, `host` 1, `tidy` 1).

By type, all 246: `sysctl` 63,
`ssh_config` 49, `sshd_config` 36, `concat_fragment` 20, `ini_setting` 20,
`augeas` 18, `concat_file` 6, `mount` 5, `anchor`, `cron`, `group` and
`reboot_notify` 4 each, `user` 3, `kernel_parameter` 2, and one each of
`host`, `init_ulimit`, `runlevel`, `selinux_state`, `simp_file_line`,
`sshd_config_subsystem`, `svckill` and `tidy`.

169 of the 246 (69%) are
Augeas-backed: `sysctl`, `ssh_config`, `sshd_config`, `augeas`,
`kernel_parameter` and `sshd_config_subsystem`. With `user` and `group` native
there, 239 remain.

`concat` is now native there too (concat 9.1.0): the SIMP gate
asserts the 9.1.0 fingerprint and fails if either type is named in
`applied via extension host`, but prints no count. The 26 concat resources
(20 `concat_fragment`, 6 `concat_file`) leaving the host list is derived from
the by-type list above, so 213 remain (239 - 26).

`sysctl`, `ssh_config`,
`sshd_config` and `sshd_config_subsystem` are native there too (augeasproviders
core 4.2.0, sysctl 4.1.0, ssh 7.0.0, openvox's libaugeas 1.15.0): the SIMP gate
asserts the twelve fingerprints of these four types (fifteen once `augeas`'s three files are added, below) and fails if any of the four is named in
`applied via extension host`, but prints no count. The 149 resources leaving
the host list (63 `sysctl`, 49 `ssh_config`, 36 `sshd_config`, 1
`sshd_config_subsystem`) are counted from the catalog the gate compiles, by
`type`, and the 64 that remain (213 - 149) are derived, not measured: `ini_setting` 20,
`augeas` 18, `mount` 5, `anchor`, `cron` and `reboot_notify` 4 each,
`kernel_parameter` 2, and one each of `host`, `init_ulimit`, `runlevel`,
`selinux_state`, `simp_file_line`, `svckill` and `tidy`.

`augeas` is native there
too (augeas_core 2.1.0): the gate adds it to the fingerprint and `applied via
extension host` assertions and prints no count. Its 18 resources leaving the
host list are counted from the same catalog by `type`, so 46 remain (64 - 18,
derived): `ini_setting` 20, `mount` 5, `anchor`, `cron` and `reboot_notify` 4
each, `kernel_parameter` 2, and one each of the seven singletons above.

`cron` is
native there too (cron_core 2.1.0): the SIMP gate checks the three fingerprints
and fails if `cron` is named in `applied via extension host`, and prints no
count. Its 4 resources leaving the host list are counted from the same catalog by
`type`, so 42 remain (46 - 4, derived): `ini_setting` 20, `mount` 5, `anchor` and
`reboot_notify` 4 each, `kernel_parameter` 2, and one each of the seven
singletons.

The Augeas family's remainder is `kernel_parameter` (2 of the 42), which stays
with the extension host by decision (2026-11-22): it saves `/etc/default/grub`
and then runs `grub2-mkconfig --update-bls-cmdline`, which rewrites the boot
configuration, and real `grub2-mkconfig` cannot run in the rootless gate
containers (`grub2-probe` fails on fuse-overlayfs), so Ruby's behaviour was
measured only against a shim that prints a stub cfg
(`2026-11-21-augeas-measurements` M36–M41). A native port could be gated only
on the `/etc/default/grub` edit and the command line, never on the boot
configuration it produces; for two resources that is not worth an ungated
boot-config writer. Revisit with a VM gate that runs real grub.

`host` and `mount` are native there too (host_core and mount_core 2.1.0; D-98):
the SIMP gate checks their fingerprints, and `sshkey`'s with them, and fails if
`host`, `mount`, `sshkey` or `ssh_authorized_key` is named in `applied via
extension host`.

Task 25's SIMP run re-measured the host list from the gate's
`applied via extension host` lines: 36 resources remain (42 - 1 `host` - 5
`mount`), not the derived 42. They are `ini_setting` 20, `anchor` and
`reboot_notify` 4 each, `kernel_parameter` 2, and one each of `init_ulimit`,
`runlevel`, `selinux_state`, `simp_file_line`, `svckill` and `tidy` (`tidy` is a
core type, the other nine are module types).

Against the first count of 246,
that is 210 fewer: 63 `sysctl`, 49 `ssh_config`, 36 `sshd_config`, 26 concat, 18
`augeas`, 7 `user` and `group`, 4 `cron`, 6 `host` and `mount`, 1
`sshd_config_subsystem`.

## `user` and `group`

`user.User` (`internal/units/user/user.go`, `user_params.go`, `user_apply.go`) and
`group.Group` (`internal/units/group/group.go`) reproduce openvox 8.33.0's `useradd` and
`groupadd` providers, measured on CentOS Stream 10 (M1–M45, 2026-11-19,
openvox 8.33.0).

- **Routing.** The user and group units' `Route`s
  (`internal/units/user/manifest.go`, `internal/units/group/manifest.go`) are
  asked once per run by `unit.RouteAll`, which `nativeProviders`
  (`cmd/voxagent/apply.go`), the one place natives are listed for `apply` and
  `agent`, calls.
  - Each type needs the host to match its own manifest's `Platforms`
    (`ral.OnPlatform`; today both list RHEL family: `ID` `rhel`, or `rhel` among
    `ID_LIKE`'s words, not Fedora, with `VERSION_ID`'s major at least 10), and
    Puppet's libuser feature off (`/usr/sbin/luseradd` and `/usr/sbin/lgroupadd`
    not both executable).
  - os-release is read from `/etc/os-release`, falling back to
    `/usr/lib/os-release` only when the former is missing (`ENOENT`, including a
    dangling symlink; `readOSRelease` in `internal/ral/platform.go`); any other
    error, such as `EACCES`, is not masked. An unreadable or unparsable
    os-release delegates.
  - Then each type is decided on its own: it is delegated when any resource of
    it names a `provider` other than `useradd`/`groupadd`, and `User` also when
    any `User`'s `purge_ssh_keys` is not `false`, `"false"` or `[]`, because
    purging generates `Ssh_authorized_key` resources only the host can apply.
  - A type is also delegated when any resource of it has a value shape the
    native parser does not implement (`rubyvalue.UnimplementedValue`: a `gid` array, a
    non-string `comment`, a number among `members`); a value Ruby itself
    refuses stays native and is refused with Ruby's message.
  - Ruby's attribute set depends on the host's provider features, so elsewhere
    the host is the oracle (D-87).
- **Attributes.** Every attribute the Ruby types declare is accepted, so
  pre-flight treats every one as known.
  - Those whose provider feature is
    missing (`user`'s `roles`, `auths`, `profiles`, `keys`, `project`,
    `attributes`, `salt`, `iterations`, `loginclass`, `ia_load_module` and
    `forcelocal`; `group`'s `attributes`, `ia_load_module` and `forcelocal`) are
    dropped as
    `type.rb:788-791` drops them, each with Ruby's `Provider <p> does not support
    features <f>; not managing attribute <a>` debug line, logged while the
    catalog is converted (`ral.ConversionDebugging`).
  - Values are validated before
    anything is applied, with Ruby's texts, and numeric strings are read with
    Ruby's `Integer()` (`'0010'` is uid 8).
- **State.** Accounts are read through libc NSS with cgo
  (`internal/lib/nameservice/nss_linux.go`, the only cgo of the user and group units): `getpwnam_r`,
  `getpwuid_r`, `getgrnam_r`, `getgrgid_r`, `getspnam_r`, `getgrouplist`, and a user
  and a group enumeration under a process-wide mutex. Buffers grow on `ERANGE` up to 1 MiB,
  then the property fails. As non-root `getspnam_r` is refused and the shadow
  fields read as absent, as Ruby's nil.
- **Commands.** shadow-utils, found on Ruby's default `PATH` order
  (`nameservice.ShadowPath`), run as argv with `LANG=C LC_ALL=C` through the same path as
  every other provider's commands.
  - When any property is Sensitive (password
    always is), every create command is logged and reported as `[redacted]`; on
    modify only a Sensitive property's own command.
  - The changed password is written to `chpasswd` on stdin; the create path
    sets it with `usermod -p`, as Ruby does (I-34).
  - A name beginning with `-`
    reaches shadow-utils as an option, as under Ruby (I-33).
- **Messages.** Every `user` change but `ensure`, and `group`'s `members`
  change, sets `Change.DisplayVerbatim`, which governs the failure line only
  (the provider boundary, above).

Unit tests use a fake command runner and a fake NSS seam
(`internal/units/user/user_apply_test.go`, `internal/units/group/group_test.go`); the root-only
`nss_linux_test.go` reads real entries it creates. G5
(`tests/spine/report-diff-container.sh`) compares the measured cases against
Ruby, live and, from fresh containers, under `--noop`; the extension gate holds the mixed-routing case, and the SIMP gate
applies SIMP's three users and four groups natively. D-87 and D-88 are theirs.

## `cron` and the ParsedFile engine

`croncore.Cron` (`internal/units/cron_core/`: `cron.go` the identity and attribute set, `cron_params.go` validation and
munge, `cron_grammar.go` the crontab grammar, `cron_apply.go` the run) reproduces the `crontab` provider of
puppetlabs-cron_core, releases 1.3.0 (the one openvox 8.33.0 vendors) and 2.1.0 (the one SIMP pins), measured with
cronie 1.7.0-15.el10 on openvox 8.33.0 (M1–M14, 2026-11-22). Line references in the code are `type:<n>`
(`puppet/type/cron.rb`), `prov:<n>` (`provider/cron/crontab.rb`), `cft:<n>` (`provider/cron/filetype.rb`'s crontab
filetype), `uft:<n>` (`util/filetype.rb`'s flat filetype), `fp:<n>` (openvox's `util/fileparsing.rb`) and `pf:<n>`
(`provider/parsedfile.rb`).

- **Routing.** D-94. `Route` (`internal/units/cron_core/manifest.go`) delegates with the first failing condition's reason:
  platform, `crontab` on Puppet's `PATH` (`command.RubyWhich`), `provider`, plugin tree (`pluginRelease`), purge
  (`purgesCron`), value shapes (`parseCron`'s `rubyvalue.UnimplementedValue`).
  - The manifest requires `group`, whose
    `ForeignProvider` it calls.
  - A tree with none of the three files is release 1.3.0, which Ruby loads from openvox
    (M10); a tree with any must match a release in full.
  - The release's `Version` goes to the provider and selects the
    rows below.
- **Releases.** Every other rule holds for both. `release130` in `cron_params.go` is the switch.

  | Point | 1.3.0 | 2.1.0 |
  |---|---|---|
  | `numfix` (`cronParser.numfix`) | an Integer, or a String with a digits-only line | also any String `rubyvalue.Integer` accepts, an out-of-int64 literal included (`ErrRange`): `"0x1f"`, `"1_000"`, `" 1"` |
  | `limitcheck`'s `to_i` | the decimal value | Ruby's `String#to_i` (`rubyvalue.ToI`): `"0x1f"` is 0, `" 1"` is 1 |
  | time field whose value is neither a String nor an Integer (`true`, `5.5`, `nil`) | delegated (D-94) | refused: `Munging failed for value true in class weekday: true is not a valid weekday`; a Hash or a whole Float ≥ 2^53 is delegated in both releases |
  | `environment` validate | a non-String is delegated | a non-String is refused: `Invalid environment setting 5` |
  | job with no environment lines (`collapseNames`) | `absent`, so `environment => []` is never in sync and every run logs `defined 'environment' as ` | `[]`, in sync |

  Refusal texts:
  - Quote a value as Ruby's `inspect` does under the agent's `default_external`
    (`rubyvalue.ExternalEncoding`, `rubyvalue.InspectIn`): `"é"` under US-ASCII, raw under UTF-8.
  - Show the tail of a time-field message with the value bare (`rawTail`), as Ruby does (M12).
  - Show a Sensitive attribute as `[redacted]`.
- **The engine.** `internal/lib/parsedfile` is a library with no knowledge of catalogs or units; cron is its first
  consumer.
  - Its FileParsing layer (`grammar.go`, `split.go`) ports `fileparsing.rb`: a `Grammar` of `LineType`s turns
    text into `Record`s and back, and is pinned by `testdata/vectors.json`, whose expected values come from running the
    real Ruby (`testdata/fileparsing_vectors.rb`) and which `vectors_test.go` and a Ruby parity test both read.
  - Its
    ParsedFile layer (`parsedfile.go`) ports `parsedfile.rb`: a `Class` holds what a provider overrides (grammar,
    default target, filetype factory, `PrefetchHook`, `ResourceForRecord`, `Adopt`, `Targets`, `ToFile`, header) and a
    `Run` holds one run's records, modified targets and backup marks (D-96): `Prefetch` reads every target and binds
    records to resources, `MarkModified` and `Flush` write the modified targets in sorted order, backing each up once.
  - Its filetypes (`filetype.go`) are `NewCrontab` (`crontab -l` and `crontab -`) and `NewFlat` (a file written through a
    temporary file; built and tested for the next consumer, which cron does not use).
  - Patterns are RE2: never write
    `\s` or `\S` in one, as Ruby's `\s` includes `\v` and RE2's does not, but `parsedfile.Space` and `NotSpace`; and a
    grammar pattern sees one line, so its anchors are Ruby's line anchors, while a type-level pattern run on a whole
    value is compiled `(?m)` (`TestNoRubyWhitespaceEscapes` scans cron_core's sources for the first mistake).
- **Reading.** Targets are the default target (`$USER` when set, even empty, else `root`: `unit.Env.LookupEnv` tells
  the two apart, where `Environ` does not), every `target` a resource gives, and every file in `/var/spool/cron` the
  effective uid can write (`withSpoolTargets`).
  - Each target's user is resolved by `nameservice.PosixUID`, which is
    `Puppet::Util.uid` (`get_posix_value`) over `User`, `UserByID` and `AllUsers`, so a user an earlier resource created
    has a uid (M11).
  - A resource with neither `user` nor `target` gets the invoker's passwd name, looked up once through
    `UserByID` (`typeTargetDefault`).
  - Prefetch happens at the first `Cron` the walk reaches (`PrefetchAtTurn`, M11).
  - A
    parse error, an unlistable spool directory and a failed hook fail the prefetch, and with it every `Cron`.
  - A
    target whose read fails (`parsedfile.ReadError`: `crontab -l` failing, or output past the D-80 cap) is remembered, and
    each flush of it fails with `Failed to read <t>'s records when prefetching them` (M1).
- **Running.** `Apply` is `resource_harness.rb`'s order: `ensure` first, and when it changes nothing else is synced;
  otherwise each out-of-sync property in type order, with Ruby's `defined`, `undefined` and `changed` messages
  (`DisplayVerbatim`).
  - A resource with a change is then flushed inside `Apply`, and a flush failure is a
    `ral.FlushError` after the changes (I-35).
  - Under `--noop` nothing is stored or written.
  - A create without `command`
    logs `no command specified, cannot create`, reports `created` and writes nothing for the job.
- **Log order (no new row).** The flush runs inside `Apply`, so a changed resource's `Flushing cron provider target <t>`
  debug line, and a filetype's pre-logged `err` (D-97), come before its change notices, where Ruby logs the notice
  first (M7). That falls under the standing policy that log order relative to a resource's own lines is not
  reproduced (`internal/transaction/transaction.go`; the gates compare a multiset, `tests/spine/allowlist.json`);
  the content is Ruby's.
- **Kept from Ruby.** Parity is the contract, so these stay; each was run on the oracle with real cronie (M9, M12, M13).
  `crontab -` validates what it is given, so several values Ruby sends never reach a crontab.
  1. The exit status and output of `crontab -` are not checked: a refused install reports success (`created` or the
     change) on every run, and the refused record stays in the run, so every later flush of that target is refused too.
  2. A create without `command` logs its `err`, reports `created`, and rewrites the target without the job.
  3. `# Puppet Name: j2   ` names the record `j2   `, which never matches `Cron[j2]`, so a second `j2` is added.
  4. Environment lines migrate: after a named job with no environment lines they attach to the next job or vanish;
     a `# Puppet Name:` with no job is dropped; unnamed comments and blanks between a name and its job move above the
     name line.
  5. Adoption is silent and the record stays unnamed.
  6. Under 1.3.0, `environment => []` on a job with none is never in sync.
  7. `user` ≠ `target`: the job is removed from the old target and written nowhere.
  8. Each changed resource rewrites its whole target.
  9. `# HEADER` is stripped mid-line on read, splicing lines.
  10. `environment => 'absent'` sends a literal `absent` line, which cronie refuses (`bad minute`), so every run reports
      `created`; a crontab edited by hand to hold it fails every `Cron` on the next prefetch.
  11. A time field `[]` sends an empty field and `command => '  '` an empty command; cronie refuses both.
  12. `'05'` and `'5'` differ; month `'___'` munges to `0`, and ranges and steps are not range-checked: cronie refuses
      `0`, `5-70` and `*/0`.
  13. The `TZ=` line is hoisted above the header, removed at its first literal occurrence.
  14. With duplicate names the later record wins.
  15. One unparsable line in any read crontab, an unrelated spool file included, fails every `Cron`, and its line
      number is one high (cronie refuses such a line at install, so it comes from a direct file write).
  16. `$USER` picks the default target.
  17. Under 2.1.0 a time value `Integer()` accepts but cron does not mean is kept verbatim: `minute => '0x1f'` is
      refused by cronie, and `' 1'` is written as ` 1`, reads back as `['1']` and is never in sync (M12).
  18. A line break in the name, `command`, an `environment` entry, `special` or a time-field element is written
      verbatim and adds crontab lines (I-38: the type's patterns are `(?m)`, so one matching line passes the value).
- **Tests and gates.** `internal/units/cron_core/*_test.go` (the Ruby vectors in `cron_params_test.go`, Ruby's
  messages in `cron_apply_test.go`, a fake `crontab` throughout), `internal/lib/parsedfile/*_test.go`, and the
  fixture copies of the real Ruby under `internal/units/cron_core/testdata`, whose digests `TestFixturesMatchManifest`
  holds to the manifest. G5's cron half and the SIMP gate are covered in [G5's cron half](gate-evidence.md#g5s-cron-half) and [The SIMP gate](gate-evidence.md#the-simp-gate).

## `host`

`hostcore.Host` (`internal/units/host_core/`: `host.go` the identity and attribute set, `host_params.go` validation and
munge, `host_grammar.go` the `/etc/hosts` grammar, `host_apply.go` the run) reproduces the `parsed` provider of
puppetlabs-host_core, measured on openvox 8.34.0 (M4–M6, M16, 2026-11-23). It is the ParsedFile engine's second
consumer and the first of the flat filetype (`parsedfile.NewFlat`). Line references in the code are `type:<n>`
(`puppet/type/host.rb`), `hp:<n>` (`provider/host/parsed.rb`), `pf:<n>` (`provider/parsedfile.rb`), `list:`
and `olist:` (`property/list.rb`, `property/ordered_list.rb`).

- **Routing.** `Route` (`internal/units/host_core/manifest.go`) delegates with the first failing condition's reason:
  platform (the unit's `Platforms`, `ral.OnPlatform`), `/etc/hosts` existing, following symlinks (`ral.Platform.FileExists`, the
  provider's `confine exists:`), `provider` (`group.ForeignProvider`), plugin tree (`checkTree`), purge
  (`unit.ResourcesPurge`), value shapes (`parseHost`'s `rubyvalue.UnimplementedValue`).
  - Without `/etc/hosts` Ruby never
    reaches the provider: `target`'s default calls `ancestors` on a nil default provider and the whole run fails with no
    report (M16), so delegating hands the host that outcome unchanged.
- **The one release.** openvox 8.34.0 vendors host_core 1.3.0, and SIMP pins 2.1.0, whose `puppet/type/host.rb` and
  `puppet/provider/host/parsed.rb` are the same bytes, so `Manifest` lists one release, `1.3.0`, with both digests. A
  tree with neither file is the vendored release; a tree with either must match both; and `puppet/provider/host/` may
  hold no `.rb` but `parsed.rb` (`another host provider in libdir`).
- **Validation.** Ruby's texts, each a whole-run refusal at catalog conversion (`host_params.go`, M6): `Invalid IP
  address <inspect>` (`validV4`, or type:22's IPv6 pattern, `v6Pattern`; a line break fails either), `Invalid host name` (`splitDots` is `String#split('.')`),
  `Hostname cannot include newline` (the ref holds the raw newline), `Host aliases cannot include whitespace`, `Host
  aliases cannot be an empty string. Use an empty array to delete all host_aliases ` (trailing space kept), `Comment
  cannot include newline`.
  - A `host_aliases` given as one string is one element: `'a  b'` is refused, `'single'` is one
    alias (M6).
  - A line break in `ip`, the name, an alias or the comment is refused, so no host value writes one: I-39's
    host cases are these refusals (M13).
- **Reading and writing.** The target is `/etc/hosts` unless `target` says otherwise, read and written through
  `parsedfile.NewFlat` with `file.BucketBackup` and `file.RelabelHook` (`host_apply.go:Prefetch`).
  - Comment, blank and
    `incomplete` lines are kept byte for byte; `incomplete` matches every line that is no host record (`incompleteMatcher`:
    Ruby's lookahead can never match).
  - An existing target keeps its mode, owner and inode; a missing one is created 0600
    with no backup (M4).
  - Each write backs the target up once per run (M5).
  - A create without `ip` logs `created`, then the
    flush fails `ip is a required attribute for hosts` (`hostToLine`) and the target is not written (M6).
- **Running.** As cron's: `ensure` first, then each out-of-sync property, with Ruby's messages (`ip changed`,
  `host_aliases changed <is> to '<should>'`, `defined 'host_aliases' as`, `comment changed '' to`); a changed resource
  is flushed inside `Apply`. The run is `ral.ParsedFileRun` (`internal/ral/parsedfile_provider.go`, shared with
  sshkey); `host_apply.go` supplies host's properties, retrieve, change text and setter as `hostEnsurable`.
- **Kept from Ruby.** Parity is the contract, so these stay (spec §8 items 1–4):
  1. Every flush rewrites the whole target with a fresh header, so one changed host rewrites `/etc/hosts` even when its
     line is the only change. Parsed lines are re-serialised (fields tab-separated, aliases space-joined), and a
     hand-written `# HEADER…` line is dropped (M4).
  2. `host_aliases => []` on a host with aliases writes `ip\tname\t` (and `\t\t# c` with a comment): `""` is not
     absent to `to_line` (`hostToLine`). The rerun is in sync; the reread aliases are absent, so a later non-empty
     list reports `defined 'host_aliases' as '…'` (M6).
  3. A comment of `''` and no comment are the same: `normalizeHost` turns an absent comment into `""`, and `# ` alone
     at a line's end parses to `""`.
  4. Names split on `.` with trailing empty parts dropped, so `a.` is valid and written `192.0.2.1\ta.` (M6).
- **Root writes through a symlinked target.** The flat filetype copies the new text over the target with
  `O_WRONLY|O_CREATE|O_TRUNC`, following a symlink there, as Ruby's `FileUtils.cp` does (`parsedfile/filetype.go`,
  `flat.Write`; spec §11). With the default target only root controls the path. A `target` in a directory another
  user can write lets that user point root's write at any file, as in Ruby; nothing refuses it.
- **Tests and gates.** `internal/units/host_core/*_test.go` (Ruby's texts in `host_params_test.go`, the shared grammar
  vectors in `host_grammar_test.go`, the run in `host_apply_test.go`, routing in `route_test.go`) and [G5's host half](gate-evidence.md#g5s-host-half)
  above.

## `sshkey`

`sshkeyscore.Sshkey` (`internal/units/sshkeys_core/`: `sshkey.go` the identity and attribute set, `sshkey_params.go`
validation and munge, `sshkey_grammar.go` the known_hosts grammar, `sshkey_apply.go` the run) reproduces the `parsed`
provider of puppetlabs-sshkeys_core, measured on openvox 8.34.0 (M3, M4, M7, M13, M17, 2026-11-23). Like `host` it
reads and writes through the flat filetype and runs on `ral.ParsedFileRun` (`internal/ral/parsedfile_provider.go`),
supplying its properties, retrieve, change text and setter as `sshkeyEnsurable`. Line references in the code are
`type:<n>` (`puppet/type/sshkey.rb`), `sp:<n>` (`provider/sshkey/parsed.rb`) and `pf:<n>` (`provider/parsedfile.rb`).

- **Routing.** `Route` (`internal/units/sshkeys_core/manifest.go`) delegates with the first failing condition's reason:
  platform (the unit's `Platforms`, `ral.OnPlatform`; the type has no confine), `provider` (`group.ForeignProvider`), plugin tree
  (`typeRelease`), purge (`unit.ResourcesPurge`), value shapes (`parseSshkey`'s `rubyvalue.UnimplementedValue`,
  under the release found).
- **Two releases.** openvox 8.34.0 vendors sshkeys_core 2.5.0; SIMP pins 3.1.0.
  - Their providers are the same bytes and
    their `sshkey` types differ only in `key`'s validation, which 3.1.0 adds (`Key must not contain whitespace:
    <inspect>`, a line break included).
  - A tree with neither of `puppet/type/sshkey.rb` and
    `puppet/provider/sshkey/parsed.rb` is the vendored release; a tree with either must match one release's two digests
    (`unit.MatchFiles`), and `puppet/provider/sshkey/` may hold no `.rb` but `parsed.rb` (`another sshkey provider in
    libdir`).
  - The release found is passed to the provider and selects only that check.
- **Identity.** The title is `<name>@<munged type>`, so `sshkey { 'foo': type => rsa }` reports as
  `Sshkey[foo@ssh-rsa]`, and a title `bar@rsa` gives name `bar`, type `ssh-rsa` (M7). `type`'s aliases (`rsa`, `dsa`,
  `ed25519`, `ecdsa-sk`, `ed25519-sk`) munge to the full names. `Sshkey` implements `ral.Retitling`
  (`PuppetRef`, `Unresolvable`) and `ral.Aliasing` so reports and relationships name the resource as Ruby does.
- **Validation.** Ruby's texts, each a whole-run refusal at catalog conversion naming the resource as Ruby has built
  it so far (`Sshkey[@ssh-rsa]` for a name failure, `Sshkey[@]` for a type failure; `sshkey_params.go`, M7):
  `Resourcename cannot include whitespaces`, `No comma in resourcename allowed. If you want to specify aliases use the
  host_aliases property`, `Aliases cannot include whitespace`, `Aliases must be provided as an array, not a
  comma-separated list`, `Invalid value "<v>". Valid values are ssh-dss, …`, `Invalid value "<v>". Valid values are
  present, absent.` (`ensure`) and, under 3.1.0 only, the `key` text above.
  - A `host_aliases` given as one string is one alias.
  - A resource with no `type` is not refused: it is
    `Sshkey[<name>@]`, logs `created`, then the flush fails `Field 'type' is required` and the target is not written; with
    `ensure => absent` it finds no record, logs `Nothing to manage`, and the line stays (M7, M17).
- **Reading and writing.** The target is `/etc/ssh/ssh_known_hosts` unless `target` says otherwise, read and written
  through `parsedfile.NewFlat` with `file.BucketBackup` and `file.RelabelHook`. Comment and blank lines are kept. A
  new target is `0644` less the umask (`newFileMode`; M4, M17); an existing one keeps its mode, owner and inode and
  gains the header. A missing directory is not created: `Puppet::Util::FileType::FileTypeFlat could not write <t>: No
  such file or directory @ rb_sysopen - <t>`, then `Could not evaluate: <same>` on the resource (M4).
- **Running.** `ensure`, then `key`, `host_aliases`, `target` (`sshkeyProperties`), with Ruby's messages (`key changed
  'A' to 'B'`, `host_aliases changed ['a1', 'a2'] to ['a2', 'a1']`). `host_aliases` is in sync only element by element
  and in order, so a reorder is a change; `[]` writes the bare name and is in sync against a line with no aliases.
- **Kept from Ruby.** Parity is the contract, so these stay (spec §8 items 1, 5, 6 and 16):
  1. Every flush rewrites the whole target with a fresh header (as `host`).
  2. A line with leading whitespace aborts every `Sshkey`'s prefetch: `Could not prefetch sshkey provider 'parsed':
     undefined method `split' for :absent:Symbol` (ruby 3.2.11's text, `absentNameError`), and each `Sshkey` of the run
     is `Skipping because provider prefetch failed`; other types still apply and the file is untouched (M7).
  3. The key field runs to the end of the line, so it swallows a trailing comment: a manifest `key` that includes the
     comment is in sync, and setting the bare key drops the comment from the file. Any write re-serialises every
     record with single spaces, so tabs between fields become spaces on every line (M7).
  4. Under 2.5.0 a `key` holding whitespace or a line break is written verbatim (`foo ssh-rsa a b`). A line break adds
     lines to a root-read known_hosts: the next prefetch parses the injected text as records of their own with no
     error, and the resource is re-applied, appending the lines again, on every run (M13, I-39 in
     [improvements.md](improvements.md)). 3.1.0 refuses both.
- **Root writes through a user's target.** The write follows a symlink at the target, as `host`'s does (`flat.Write`;
  spec §11). With the default target only root controls the path. An `sshkey` `target` in a user's home, or any
  directory another user can write, lets that user point root's write at any file by replacing the target with a
  symlink, as in Ruby; nothing refuses it.
- **Tests and gates.** `internal/units/sshkeys_core/*_test.go` (Ruby's texts in `sshkey_params_test.go`, the shared
  grammar vectors and the leading-whitespace abort in `sshkey_grammar_test.go`, the run in `sshkey_apply_test.go`,
  routing and both releases in `route_test.go`) and [G5's sshkey half](gate-evidence.md#g5s-sshkey-half).

## `ssh_authorized_key`

`sshkeyscore.Sak` (`internal/units/sshkeys_core/`: `sak.go` the identity and attribute set, `sak_params.go` validation
and munge, `sak_grammar.go` the authorized_keys grammar, `sak_apply.go` the prefetch, apply and flush, `sak_file.go`
the filetype that writes as the key's user) reproduces the `parsed` provider of puppetlabs-sshkeys_core, measured on
openvox 8.34.0 (M4, M5, M8, M13, M17, 2026-11-23). It runs on `ral.ParsedFileRun`, as `sshkey` does, but flushes its
own targets (below) and acts on its files as the key's user through the `voxagent asuser` helper ("Privilege
drop" below). Line references in the code are `kt:<n>` (`puppet/type/ssh_authorized_key.rb`), `kp:<n>`
(`provider/ssh_authorized_key/parsed.rb`) and `pf:<n>` (`provider/parsedfile.rb`).

- **Routing.** `routeSakType` (`internal/units/sshkeys_core/manifest.go`) delegates with the first failing condition's
  reason, decided apart from `sshkey`'s: platform (the unit's `Platforms`, `ral.OnPlatform`), `provider` (`group.ForeignProvider`),
  plugin tree (`typeRelease` on the type's own two files, `another ssh_authorized_key provider in libdir`), a
  `Resources` purge of `ssh_authorized_key`, any `User` whose `purge_ssh_keys` generates keys
  (`user.PurgeKeepsNative`: the generated resources would edit the same files from the extension host), then value
  shapes (`parseSak`'s `rubyvalue.UnimplementedValue`).
- **Two releases.** 2.5.0 (vendored) and 3.1.0 (SIMP) differ in digest only: the type's hunks are hash shorthand, a
  comment and a dropped `.freeze`, and the providers are the same bytes, so the release found changes no behaviour.
- **Identity and validation.** The name is the title unless `name` is set; a refusal names the resource by its title
  either way (`Ssh_authorized_key[t]` for title `t`, name `n`; M17). Ruby's texts, each a whole-run refusal at
  catalog conversion (`sak_params.go`, spec §4.3): `Invalid value "<v>". Valid values are true, false, yes, no.`
  (`drop_privileges`), the `type` list of 14 values with its aliases, `Key must not contain whitespace: <value>`
  (raw), `Option <v> is not valid. …`, and `Attribute 'user' or 'target' is mandatory` when neither is set.
- **Target.** Unset, the target is `~<user>/.ssh/authorized_keys` from the passwd entry, looked up when
  asked, so a `User` created earlier in the run counts; with no such user it is nil and Ruby logs debug `The required
  user is not yet present on the system` at every ask (spec §6.3; never count it). `User[<user>]` is autorequired
  when managed. A given `target` is a path used literally, even `absent` (a String like any other), and
  `target => []` leaves none.
- **Reading.** Comment and blank lines are kept.
  - A `parsed` line is `[options] type key [name]`; a `key_v1` line is
    `[options] bits exponent modulus [name]`; anything else fails the prefetch `Could not parse line "<line>" (file:
    <target>, line: <n>)` and every `Ssh_authorized_key` of the run is `Skipping because provider prefetch failed`.
  - A `parsed` line with no name is named `<target>:unnamed-<n>` per target in file order (debug `generating name for
    on-disk ssh_authorized_key <key>: <name>`), and a resource of that title manages it.
  - Reads and the backup run as
    root in the parent, following symlinks, as Ruby's prefetch does (I-40 in [improvements.md](improvements.md)).
  - A
    record is bound by name across every target the run reads: the default and the catalog's `ssh_authorized_key`
    targets.
- **Running.** `ensure`, `type`, `key`, `user`, `target`, `options` (`sakProperties`), with Ruby's messages (`options
  changed ['absent'] to ['no-pty', 'command="x y"']`, `type changed 'ssh-rsa' to 'ssh-ed25519'`, `key changed 'K1' to
  'K2'`, `target changed '<old>' to '<new>'`); all changes of one resource are one flush. `user` is always in sync.
  A `Sensitive` `key` is written in clear and shown `[redacted]` in messages and events (D-78).
- **Flush** (`Sak.flush`, `kp:69-103`, spec §6.3), in this order: no `user` fails `Cannot write SSH authorized keys
  without user`; a user with no uid fails `User '<user>' does not exist`; the target (after a move, the new one) is
  backed up in the root parent, so a refused write still leaves a bucket entry. A move's old target is backed up in
  the root parent too, once per prefetch, where Ruby backs it up as the new user (D-100). Then:
  - `drop_privileges` true (the default): `mkdir(dirname(target), 0700)` when missing (debug `Creating <dir> as
    <user>`), the write of the record's own targets, then `chmod 0600` on the target, each as the user: in the helper
    when the agent is root and the user's uid is not its own, in-process otherwise. A refused write is logged twice,
    as Ruby logs it (`Puppet::Util::FileType::FileTypeFlat could not write <path>: <errno> @ rb_sysopen - <path>`,
    the symlink's path, then the resource's `Could not evaluate:`); a refused `mkdir` (`@ dir_s_mkdir`) or `chmod`
    (`@ apply2files`) once.
  - `drop_privileges` false: `trustedPath` (`sak_file.go`, `kp:53-67`) must pass for every own target, or the flush
    fails `drop_privileges is false but the target path is not trusted` and nothing is written. Then the targets are
    written as root and the target is chowned to the user's uid and the gid of the group named like the user (`-1`
    when none) and set `0600`. A directory target fails the prefetch (`Is a directory @ io_fread`).
- **Own targets (D-102).** A flush writes only the targets its own record touched: its target before a change (a
  move's old target) and its should target (`parsedfile.Run.FlushTargets`). Ruby also writes a target another
  resource's failed flush left modified, under the next resource's identity; voxagent does not (I-41).
- **Kept from Ruby.** Parity is the contract, so these stay (spec §8 items 7-11, 16 and 19):
  1. Names are unique across the read targets: a key whose name another user's file holds is moved out of that file
     (`target changed`) only when that file is some resource's target; otherwise it is created in the new file and
     left in the old (M8).
  2. A record with no name is written with a trailing space (`ssh-rsa K `).
  3. `drop_privileges => false` writes only when the target's parent exists, its ancestors pass the walk (owned by
     the agent's euid, not group- or world-writable, from the target's directory up to the one below `/`) **and**
     debug logging is on: Ruby's trusted branch returns `Puppet.debug`'s value. With debug off every such target fails
     `drop_privileges is false but the target path is not trusted`.
  4. The drop-false chown uses the group named like the user, not the user's primary group; with no such group the
     group is left as written.
  5. A target change writes the old target as the new user too, so moving a key out of another user's `0600` file
     fails `could not write <old target>: Permission denied @ rb_sysopen` and the new target is not written.
  6. A `key_v1` record never converges: a resource of its name reports `defined 'type'`, `defined 'key'` and `defined
     'options'` on every run and the line is written back unchanged. A `key_v1` line with no name fails every flush
     of its target with `Field 'name' is required`.
  7. A line break in `name` or `options` is written verbatim (I-39); the next run's prefetch fails `Could not parse
     line` until the file is repaired by hand.
- **Not kept.** Spec §8 item 18: the write of another resource's leftover target (D-102, I-41 above).
- **Tests and gates.** `internal/units/sshkeys_core/sak_*_test.go` (Ruby's texts in `sak_params_test.go`, the grammar
  and naming in `sak_grammar_test.go`, the run, flush, privilege drop, trusted walk and D-102 leak cases in
  `sak_apply_test.go`), routing in `route_test.go`, the helper in `internal/lib/asuser` and
  `cmd/voxagent/asuser*_test.go`, and [G5's sak half](gate-evidence.md#g5s-sak-half).

## Privilege drop (`voxagent asuser`)

`ssh_authorized_key` creates a directory, writes a file and sets its mode as the
key's user, where Ruby wraps those three steps in
`Puppet::Util::SUIDManager.asuser` (`kp:80-88`): the single-level
`mkdir(dir, 0700)`, the flat write, and `chmod 0600`. voxagent runs them in a
child, the hidden `voxagent asuser <op>` (`cmd/voxagent/asuser.go`, ops in
`internal/lib/asuser`), because Go cannot change one thread's credentials
(D-99).

| Op | Does, as the calling uid |
|---|---|
| `mkdir <dir>` | when `os.Stat(dir)` fails (any failure is absence, as `Puppet::FileSystem.exist?`), `mkdir(dir, 0700)` less the umask. The check runs in the child, as the user |
| `write --encoding <enc> <path>` | the flat write (`parsedfile.NewFlat(path, 0, nop, relabel, enc).Write`) of stdin: temp file (in `asuser.TempDir`, Ruby's `Dir.tmpdir` for the user, D-101), copy over the target following a symlink, then the SELinux relabel (`file.Relabel`), as the user. The backup stays in the root parent, before the drop |
| `chmod <path>` | `chmod(path, 0600)`, following a symlink |

Contract (spec §3.4):

- **Start.** `asuser.ProcStarter` runs `/proc/self/exe asuser <op> …`, the running
  root-owned inode, so a binary replaced or deleted mid-run is never what runs as
  the user; the start never falls back to `os.Executable()`. The credential comes from
  `suid.Resolve(user, nil, marked)` and is set before `execve`. Stdin is the
  text (write) or empty; cwd and umask are inherited; the environment is `PATH`
  and `TMPDIR` only (D-101). No `Executing:` line: Ruby runs no command here.
- **Refusals.**
  - The child refuses euid 0 (`asuser: refusing to run as root`, exit 2).
  - The parent never starts it for uid 0: when the agent is not root or its
    euid is already the user's, it calls `asuser.Mkdir` and `asuser.Chmod`
    in-process (`suid:65`, `:121`), and writes through the sak filetype's flat
    `Write` rather than `asuser.Write`, which only the child calls.
  - `ProcStarter.Start` also refuses a missing or uid-0 credential itself,
    starting nothing (`refusing to start the asuser helper as root`), and empty
    args.
  - Any other invocation exits 2 with no output.
  - A cancelled run's error is the context's, not the invalid result.
- **Result protocol.** The child's whole output is one line, `<step> <errno>`
  (`asuser.Result`).
  - Steps: `ok`, `mkdir-created` (errno 0, exit 0); `relabel`
    (errno 1-4095, exit 0: the file was written, the relabel failed);
    `mkdir`, `tempfile`, `open`, `copy`, `close`, `chmod` (errno 1-4095, exit 1);
    `tmpdir` (errno 0, exit 1: no temporary directory, D-101).
  - `asuser.ParseResult` accepts only that: one line of at most 64 bytes, in
    canonical form, with an agreeing exit status. Anything else, a signal, or an
    exit of 2 or more is `asuser helper for <path> returned an invalid result`,
    quoting nothing from the child.
  - stdout is capped at 65 bytes and drained; stderr
    goes to the null device; `exec.Cmd.WaitDelay` (1 s) stops a process the child
    left holding stdout from hanging root.
- **Rendering.** The parent renders every message from the result, with the
  in-process texts: `parsedfile.FlatWriteError(path, WriteFailure{step, errno})`
  for `tempfile`, `open`, `copy`, `close` and `tmpdir` (the same formatter `flat.Write` uses),
  `parsedfile.RubyErrno` for `mkdir` (`@ dir_s_mkdir - <dir>`) and `chmod`
  (`@ apply2files - <path>`), debug `Creating <dir> as <user>` for
  `mkdir-created` and `mkdir <errno>`, and for `relabel` the warning `Failed to
  set SELinux context <c> on <f>`, `<c>` looked up by the parent with
  `file.DefaultContext`.
  - The encoding failure has no step: the parent checks
    `parsedfile.FlatEncodeError` before it starts the write (D-101); a child that
    meets it anyway exits 2.
  - A valid result whose step the op cannot report
    (`chmod 1` for a mkdir) is the invalid result too.
  - The sak flush
    (`internal/units/sshkeys_core/sak_file.go`) renders these, with a marked
    `user` shown `[redacted]` (D-78) and paths raw, as Ruby shows them (they are
    on the helper's argv anyway); the credential is set for one flush and
    cleared after.
  - No relabel warning when the parent finds no default context:
    Ruby attempts no relabel then.

**Trust model (spec §11).** The helper gains no privilege: it is the target
user from its first instruction, is not setuid, accepts only the three ops, and
refuses root, so a user who runs it directly can do only what they already
could. Every path component of the mkdir, the temp file, the copy and the chmod
is checked by the kernel as that user, so a symlink the user plants towards a
root file fails `open 13` where a root write would succeed
(`TestAsuserRootSymlinkIntoForeignDir`).

Unlike Ruby's in-process drop (real uid
0), the helper can be ptraced by its user, who then controls its output. That is
why the output is untrusted input: a forged result can only claim that a step
succeeded or failed with some errno, which the user could cause anyway through
their own file permissions. It cannot put text into root's log or report.

The
target path is on argv, visible in `/proc/<pid>/cmdline`; the content is only on
stdin.

**Running the root tests.** `go test -c -o .test-output/voxagent.test
./cmd/voxagent && sudo -n .test-output/voxagent.test -test.run TestAsuserRoot`
(never `sudo go test`). They drop to `nobody` in a 0755 temp tree; the test
binary is its own helper (`TestMain` dispatches `asuser`), so `ProcStarter`
starts exactly the code under test.

## `mount`

`mountcore.Mount` (`internal/units/mount_core/`: `mount.go` the identity and attribute set, `mount_params.go` validation
and munge, `mount_grammar.go` the `/etc/fstab` grammar, `mount_apply.go` the prefetch, ensure state machine, commands,
flush and refresh, `mount_relations.go` the autorequires and autobefores) reproduces the `parsed` provider of
puppetlabs-mount_core, measured on openvox 8.34.0 (M4, M9–M13, M17, 2026-11-23). It reads and writes `/etc/fstab`
through the flat filetype, as `host` does, but `ensure` is a plain property with its own state machine (`mt:26-124`),
not ensurable, so it shares only the record binding (`ral.PrefetchParsedFile`, whose `*ral.ParsedFileRun` it holds for
the records) and not `ral.ParsedFileRun.Apply` or `ral.ParsedFileEnsurable`. Line
references in the code are `mt:<n>` (`puppet/type/mount.rb`), `mm:<n>` (`puppet/provider/mount.rb`), `mp:<n>`
(`provider/mount/parsed.rb`) and `pf:<n>` (`provider/parsedfile.rb`).

- **Routing.** `Route` (`internal/units/mount_core/manifest.go`) delegates with the first failing condition's reason:
  platform (the unit's `Platforms`, `ral.OnPlatform`), `mount` and then `umount` resolving over `PATH` as `Puppet::Util.which` does
  (`command.RubyWhich`; without either Ruby fails the whole run with `undefined method `ancestors' for nil:NilClass`
  and no report, M11, so delegating hands the host that outcome), `provider` (`group.ForeignProvider`), plugin tree
  (`pluginRelease`), purge (`unit.ResourcesPurge`), value shapes (`parseMount`'s `rubyvalue.UnimplementedValue`), and,
  when the catalog holds a `File`, every valid Mount's munged name a pattern RE2 reads as Onigmo does
  (`portablePattern`: none of `\`, `(?`, `{`, `[`).
  - Ruby interpolates the name raw into each `File`'s autobefore
    regexp, so an uncompilable name fails its whole run once a `File` exists (`end pattern with unmatched
    parenthesis`, M11); delegating such names is conservative.
  - A Mount that fails validation is left out of that check,
    because Ruby refuses it at catalog conversion, before any pattern is compiled.
- **Two releases.** openvox 8.34.0 vendors mount_core 1.3.0; SIMP pins 2.1.0. They differ only in Ruby 3.1 hash
  shorthand, so one behaviour covers both. A tree with none of `puppet/type/mount.rb`,
  `puppet/provider/mount/parsed.rb` and `puppet/provider/mount.rb` is the vendored release; a tree with any must match
  one release's three digests (`unit.MatchFiles`), and `puppet/provider/mount/` may hold no `.rb` but `parsed.rb`
  (`another mount provider in libdir`).
- **Validation.** Ruby's texts, each a whole-run refusal at catalog conversion (`mount_params.go`, spec §4.4):
  `ensure`'s and `remounts`' `Invalid value …`, `fstype`/`options` `must not contain whitespace` or `must not be an
  empty string`, `blockdevice must not contain whitespace`, `atboot`'s boolean munge failure, and `dump`'s `Invalid
  value "<v>". Valid values match /(0|1)/.`. The name is munged by stripping trailing slashes (`/mnt/t/` is
  `/mnt/t`; `/` stays `/`); the title stays in every message and report ref.
- **Reading.** `/etc/fstab` unless `target` says otherwise, read and written through `parsedfile.NewFlat` with
  `file.BucketBackup` and `file.RelabelHook`.
  - A new fstab is 0600; an existing one keeps its mode, owner and inode
    (M4).
  - `\040` in the device and name fields is a space, and a space is written `\040`.
  - Then `mount` (no arguments,
    `command.ArgvOpts{FailOnTruncate: true}`, so a listing cut by the D-80 cap fails the prefetch rather than hiding
    mounts) marks each listed name's record: `absent` becomes `ghost` (mounted, not in the fstab) and `unmounted`
    becomes `mounted`.
  - A failing or unparseable listing fails the prefetch type-wide (`Could not prefetch mount provider
    'parsed': …`, then `Skipping because provider prefetch failed`).
- **The listing is ambiguous.** The name is the greedy capture of ` on (.*) type ` (`mp:285`) on `mount`'s raw output,
  where a space is a space (M9). A device or mount point holding ` on ` or ` type ` (a user can choose both for a FUSE
  mount) is misparsed, as in Ruby. The parsed name only marks a resource `mounted` or `ghost`; every command acts on the
  resource's own name, never on a listed one (spec §11).
- **Running.** An out-of-sync `ensure` is synced alone through the state machine (spec §6.4's table: create, flush,
  `mount`, `umount`, destroy, and *syncothers*, which sets every other out-of-sync property without a change or event and flushes
  at once).
  - Otherwise, unless the current ensure is `absent`, each out-of-sync property of `device, fstype, options,
    pass, dump, target` is synced in that order and reported as a normal change; a changed resource is flushed inside `Apply`, and a
    `needs_mount` flag set by the state machine mounts after the write.
  - `ghost` counts as present: with no `ensure`
    every property is synced against the fresh record. `present` is munged to `defined`.
- **Commands.** `mount <name>`, `umount <name>` and `mount -o remount <name>` run through `command.ShadowRunner`
  (`command.RunArgvWith`: argv only, never a shell, `LANG=C`/`LC_ALL=C` with the other locale variables removed, the
  D-80 cap), as the resolved paths (`Executing: '/usr/bin/mount -o remount /mnt/x'`, D-82).
  - A non-zero exit is
    `command.ExecFailure` with util-linux's text.
  - In the ensure sync it is two errs, the bare text at source `Puppet` and
    `change from '<is>' to '<should>' failed: <text>` on `<ref>/ensure`; in the flush's mount it is `Could not evaluate:
    <text>` after the ensure change succeeded and the fstab was written.
- **Refresh.** The type refreshes itself (`ral.SelfRefreshing`, `self_refresh: true`) after any change but
  `ensure => absent`, and on another resource's notify. It remounts only when the record is mounted at refresh and the
  should `fstype` is not `swap`: info `Remounting` at `Mount[<title>](provider=parsed)`, then `mount -o remount
  <name>`, or with `remounts => false` `umount` then `mount`. A failing remount is two errs on the resource,
  `Failed to call refresh: <text>` and `<text>`, status failed, `failed_to_restart` (`ral.RefreshLogging`).
- **Relations.** `AutoRequires`: `Mount[<p>]` for each managed proper ancestor of the munged name, root first.
  `AutoRelations` (`RelBefore`): `File[<path>]` for every catalog `File` whose path matches `(?m)^<name>/.`, in catalog
  order, so a mount is before the files written on it.
- **Kept from Ruby.** Parity is the contract, so these stay (spec §8 items 12–17, 20 and 21):
  1. (§8 12) `ghost → absent` unmounts, then rewrites the fstab unchanged but for the header. A Mount without `fstype` appends
     a record whose generation fails, so every later fstab flush in the run fails with the first resource's text
     (`Could not evaluate: Mount[/mnt/f1]: Field 'fstype' is required`) and the file is not written; a *parsed* line
     without `fstype` is re-emitted with empty fields (M12, M13).
  2. (§8 13) A line with leading whitespace parses with `device` absent, so it is not found under its name, and the next
     write of that fstab fails `Could not evaluate: Field 'device' is required`, file unchanged (M12).
  3. (§8 14) `dump`'s check is unanchored: `10` and `x1y` pass and are written verbatim; `pass` has none (M12).
  4. (§8 15) A refresh remounts whenever the record is mounted at refresh, whatever `ensure` says: `absent → mounted` mounts and
     then remounts in the same run. `defined` and `unmounted` never remount; `absent` never refreshes; properties
     *syncothers* sets leave no change (M11).
  5. (§8 16) A line break in `name`, `device`, `dump` (any value holding `0` or `1`) or `pass` is written verbatim, adding
     fstab lines, so a mount at boot. The next prefetch parses the injected text as records of their own with no
     error, and the resource is re-applied, appending the lines again, on every run (M13, I-39 in
     [improvements.md](improvements.md)).
  6. (§8 17) A name starting with `-` is an option to the tool: Ruby adds no `--` and neither does voxagent. `Mount['-o']`
     `mounted` runs `/usr/bin/mount -o` and fails `option requires an argument -- 'o'` with the fstab line already
     written (M17 `17d-*`).
  7. (§8 20) Parsed `dump`/`pass` are Strings and the resource's default is Integer `0`, so a managed line with other values
     is rewritten `0 0` although the manifest sets neither (`pass changed '2' to 0`, M12).
  8. (§8 21) A 6+-field line folds its tail into `pass` with TABs; a managed resource reports `pass changed
     "0\textra\tmore" to 0` and rewrites it (M12).
- **Root writes through a symlinked target.** As `host`'s (`flat.Write`, spec §11): with the default target only root
  controls the path; a `target` in a directory another user can write lets that user redirect root's write.
- **Tests and gates.** `internal/units/mount_core/*_test.go` (Ruby's texts in `mount_params_test.go`, the fstab grammar in
  `mount_grammar_test.go`, the listing, state machine, commands, flush and refresh in `mount_apply_test.go`,
  relations in `mount_relations_test.go`, routing and both releases in `route_test.go`, and the check that the unit's
  patterns avoid Ruby's `\s` in `source_scan_test.go`) and [G5's mount half](gate-evidence.md#g5s-mount-half).

## `concat_file` and `concat_fragment`

`concat.ConcatFile` and `concat.ConcatFragment` (`internal/units/concat/concat*.go`)
reproduce puppetlabs-concat 9.1.0 and 10.0.1's two types for the plain format,
measured on CentOS Stream 10 (M1-M22 and the v10 series, 2026-11-20, openvox
8.33.0). The module's `concat` and `concat::fragment` defines are manifest code
and reach voxagent as these two types.

- **Routing.** The concat unit's `Route` (`internal/units/concat/manifest.go`, `nativeConcat` in `concat_route.go`) is asked once
  per run by `nativeProviders` (`cmd/voxagent/apply.go`) via `unit.RouteAll`, as `user` and `group`
  are.
  - It reads `puppet/type/concat_file.rb` and `puppet/type/concat_fragment.rb`
    under `--libdir` through the `PluginTree` interface (tests use a fake),
    fingerprints both with sha256, and returns the release (`ConcatVersion`) only
    when both belong to the same one.
  - Then it runs every `Concat_file` and
    `Concat_fragment` through the native parser: a shape it does not implement
    delegates **both** types (D-89).
  - Both providers carry the version, because 9.1.0
    and 10.0.1 differ (below).
  - With `--no-ruby` a native concat catalog needs no
    Ruby; a delegated one is refused as any host type is.
- **Parameters.** `parseConcatFile` and `parseConcatFragment` return either a
  Ruby refusal (a validation failure, printed with Ruby's text, `Parameter <p>
  failed on Concat_file[<t>]: ...`), which keeps the resource native, or an
  `rubyvalue.UnimplementedValue`, which routes it away. The parsers stop at a resource's
  first error, in Ruby's validation order, so a refusal earlier in a resource
  hides a later unimplemented value: the run is refused either way. A marked
  value prints `[redacted]` in these texts (D-78).
- **Generation, and `hostLoaded`.** `concat_file`'s `generate` runs at load
  time: the `File` at the `Concat_file`'s path is created (or reused when the
  catalog declares one, untouched) and the `Concat_file` is ordered before it
  unless a relationship already links them.
  - It is `ConcatFile.LoadGenerate`, a
    `ral.LoadGenerator`, run by `loadGenerate` (`cmd/voxagent/loadgen.go`) after
    pre-flight and validation, and its resources are judged as the catalog's own.
  - It runs only when `extensionIngest.hostLoaded` is false, because the host's
    `load` already ran Puppet's own `generate` on every vertex, native-claimed
    ones included, and merging twice would duplicate the `File`
    (`TestLoadGenerateSkippedWhenHostLoaded`; `TestConcatGenerationHostAndNativeAgree`
    compares the two paths).
  - The new `File` carries no `path` (the title is the
    path, and a catalog omits a namevar equal to its title).
- **Content at the turn, and the unit.** `concat_file`'s `eval_generate` runs at
  the `Concat_file`'s turn (`ConcatFile.EvalGenerate`): it collects the fragments
  that belong to it (`concatBelongs`, from either side), sorts them as Ruby does
  (`concatSort`: numeric or alpha, ties in catalog order, D-90), joins them
  and returns an override of the `File`'s `content` (and `ensure => absent` for an
  empty result with `create_empty_file => false`).
  - The `File` is returned in
    `EvalGeneration.Existing`, so `visitHostUnit` applies it as the unit's member and
    the unit's completed whit carries the event on, notifies and subscribers
    included (D-43 for what differs).
  - A raise (incomparable order keys, no
    `File`) is a failed generation, logged at the `Concat_file`.
- **Marks.** `Override.ContentMark` sets the `File`'s `content` flag as 10.0.1's
  parameter does (marked with a `Sensitive` fragment, explicitly not sensitive
  for a file with no fragments, unset otherwise); 9.1.0 sends
  `ContentMarkKeep`, leaving the content unmarked as Ruby does (M12). A new
  `File` takes the `Concat_file`'s marks on `owner`, `group` and `mode`, a
  declared one none (D-78). `Override.RedactOutput` redacts `validate_cmd`'s
  output when any fragment is marked, under both releases (D-78).
- **Orphan warnings.** `ConcatFragment` implements `ral.AutoRelationWarning`
  (`GraphWarnings`): the graph build (`internal/transaction/build.go`) logs, at
  `warning` and at the fragment's path, Ruby's `Target Concat_file with path or
  title '<t>' not found in the catalog` for each fragment no `Concat_file`
  collects, in catalog order and before the run applies anything. The second
  form's missing closing quote is Ruby's.
- **Events.** There are none of its own. A `Concat_file` and a `Concat_fragment`
  have no status; the `File` carries every event, with native `File`'s text,
  backups and `validate_cmd` (D-48).
- **Version deltas.** 9.1.0 and 10.0.1 differ in the content mark above, in
  whether a list `tag` on the `Concat_file` includes a fragment's string `tag`
  (10.0.1 only, `concatBelongs`), and in 10.0.1's `redacted: false` for a file
  with no fragments, which is kept.

Behaviour kept from Ruby that looks wrong (the parity contract): alpha mode's `__`
split, so `10___n` sorts as order `10`; Integer orders compared as strings in
alpha mode (`1000` before `90`); the orphan warning's missing quote; a declared
`File`'s `owner` and `mode` winning over the `Concat_file`'s (M14); a `File`
ordered before its `Concat_file` never receiving its content and never refreshing the `Concat_file`'s targets (M21);
`create_empty_file => false` deleting an existing file (M7); the default `backup`
not bucketing (M5); a failed generation still creating an empty file (M3c); an
empty fragment becoming a blank line under `ensure_newline` (M4); 9.1.0 not
marking a `Sensitive` fragment's file content (M12).

Unit tests use the real parsers with a fake `PluginTree`; the two release trees
themselves are committed under `internal/units/concat/testdata` (the two type
files of each, unmodified; `TestConcatTestdataMatchesManifest` ties them to
concat's `Manifest`), so the tests that read them never skip. G5's concat half (`tests/spine/concat/g5.sh`) compares 33 measured
cases (inputs committed under `tests/spine/concat/cases/`; live, rerun and `--noop`) against Ruby and the on-disk state of the
backup case, and the 14 refusals; the extension gate holds the host path
(every concat there is delegated); the SIMP gate applies its 26 concat resources
natively. D-89, D-90, D-43, D-78 and D-84 are theirs.

## `file` source, recurse and purge

`source` is read locally: an absolute path or a `file:` URI, or a list of them,
the first that exists winning. A `puppet:` source the agent has downloaded
(a key of `File.Sources`, filled by `voxagent agent`) is read from its local
copy. An unmapped `puppet:` source, `http:` and `https:` are pre-flight gaps,
each list element judged on its own.

Relative, opaque and
unparseable sources, a `file:` URI naming a host, two of `content`, `source`
and `target` (a target `ensure` supplies counts), `content` with
`recurse => true`, `ensure => link` or a link target with `recurse => true`
(Puppet's `recurse_link`), an `ensure` that is neither a string nor `false`, a
`target` that is not a string and a static catalog's inlined `metadata` are
validation refusals.

A regular-file source is checksummed when it is resolved
and streamed through sha256 when it is copied; a copy whose checksum differs --
the source changed in between -- is discarded with Puppet's `File written to
disk did not match desired checksum` message, and the destination keeps what it
had. Neither side of a comparison is read whole, so a source may be larger than
memory. A missing source fails its resource even under `ensure => absent` and
`--noop`, as Puppet's does.

Recursion is an engine capability, `ral.Generator`. A resource whose provider
generates is applied as a **unit** at its turn in the walk: `Generate` reads the
filesystem as the run has left it, the parent applies, then each child applies
in bytewise path order through the same per-resource routine, each with its own
status. Children are not graph vertices. Puppet inserts them into its graph and
moves the parent's out-edges onto a "completed" whit; voxagent reproduces the
whit's two effects from the unit's result.

A failure anywhere in the unit
blocks the parent's dependents, naming the member that failed, and otherwise the
parent's subscribers get one refresh event when any member produced one.
Generating up front was rejected because it reads the filesystem before earlier
resources run; a mutable graph because it rewrites the walk's core contract for
no observable gain.

A child depends on its nearest generated ancestor; a failed
or skipped parent still generates and its children are reported skipped. A
generation failure is logged against the parent and fails the report but not
the exit code (Puppet's `compute_status` against `exit_status`). Under `--noop`
the whit's "Would have triggered" notice is logged at the parent's title with
the unit's event count, as Puppet's whit does (D-25 retired).

`File.Generate` walks the directory and a directory source breadth first, never
descending a symlink, and drops every child at or below another catalog `File`.
Children inherit the parent's parameters except `ensure`, `recurse` and
`source`. A child that neither the source nor `purge` decides has no `ensure`
and manages only the permissions of what is there -- the rule a catalog `file`
with no `ensure`, `content` or `source` now follows too (`type/file.rb:561-567`).
Numeric modes are dirmasked on directories, so `mode => '0644'` leaves the
tree's directories 0755. More than 1,000 entries in one walk logs Puppet's
soft-limit warning and keeps every entry.

Memory is linear in the number of
changed children, because each one's status and logs are held until the report
is written: measured at about 8 KB per changed child, a first run over a
100,000-entry tree peaked at about 822 MB and the converged rerun at 159 MB.

Every operation on a child goes through an `os.Root` opened on the recursed
directory (`rootFS`), after refusing a symlink at the directory itself, so a
symlink swapped into the tree after the walk cannot carry a write, chmod, chown
or unlink out of it.

Residuals remain, all inside the tree: `os.Root` follows a
link whose target stays in the tree, so such a swap can redirect an operation
within it, including into a catalog-managed subtree `purge` exempts; there is no
`Lchmod`, so `chmod` is preceded by an `Lstat` and a swap between the two is a
race; an empty directory swapped in before a purge's remove is rmdir'd; and a
hard-linked child shares its inode, so its chmod or chown reaches the other
link, as Puppet's does. The `rootFS` doc comment lists them.

The walk reads each
directory with `O_NONBLOCK|O_DIRECTORY`, so a directory swapped for a fifo
between its Lstat and its open fails the walk with ENOTDIR instead of hanging
the agent (`internal/units/file/file_recurse_test.go:TestFileGenerateDirectorySwappedForAFifoFailsInsteadOfBlocking`).

`openDirNoFollow` does the same for the recursed directory itself, opening it
`O_NONBLOCK|O_DIRECTORY|O_NOFOLLOW` and making the `os.Root` from that handle
through `/proc/self/fd`, because `os.OpenRoot` on a path blocks on a fifo
(`TestFileGenerateRecursedDirectorySwappedForAFifoFailsInsteadOfBlocking`).

That root is named by its `/proc` path, so `rootFS` re-opens every file it hands
out under the real path (`withName`) and errors on the handle name the real
file (`TestRootFSHandleErrorsNameTheRealPath`). An unusable `/proc`
fails the child with an error other than `fs.ErrNotExist`, so `--noop` refuses
the child rather than falling back to unconfined reads (`TestFileApplyNoopRefusesAChildWhenProcIsUnusable`).

A
walked symlink keeps its mode, is `lchown`ed and relabelled itself through
`rootFS` (D-28, retired), and `purge` unlinks it. Purging a directory copies
Puppet's notice and `removed` event and leaves the directory, every run (I-1;
D-5 is retired),
so a purge over an unmanaged directory is never idempotent: G1 compares that
case and G2 leaves it out, as it leaves out the missing-source case, whose first
run fails by design. A file↔directory conflict inside a recursion follows
`force` like any other resource (D-27 is retired).

## `file` links, force and replace

The register rows hold each deviation; this section says where Puppet defines
each piece and how they fit.

**`path`.** A catalog carries `path` only when it differs from the title
(`resource.rb:120-121`). `FilePath` is the one place that resolves it, cleaned
as a title is (`type/file.rb:66-73`); report keys, event sources and refs stay
the title. A `require` naming the path resolves through `fileAliases`
(`resource/catalog.rb:176-178`), and `AutoRequires` offers `FilePath`.

**`links`.** `manage`, the default, acts on the link; `follow` acts on what it
points at (`type/file.rb:272-281, 895-903`): `Lstat` against `Stat` for the
destination. Any other value is refused before the run in Puppet's wording. How
a link at the path is replaced, written through or `lchown`ed is under
[Writing files](apply-spine.md#writing-files) (D-4, retired, and I-5). A symlink `source` becomes a link under
`manage` and its target's content under `follow` (`source.rb:148, 170-176`;
D-26 keeps the special file types). `follow` with `recurse => true` is refused
by `Validate` (D-32); a recursion child that inherits `follow` still cannot
chmod through its link (D-31), and a walked link is managed under `manage`
(D-28, retired).

**`force`.** It matters only for a directory in the way, in `removeExisting`
(`type/file.rb:1022-1049`): without it the notice `Not removing directory; use
'force' to override` is logged and the directory stays, with it the tree is
removed. A file, link, fifo or socket is always unlinked (`:814-815`) and a
block or character device fails (`:816-818`). Puppet's consequences without
`force` are reproduced and registered: `ensure => absent` reports `removed`
while the directory stays (I-1), and a link replacing a directory logs the
notice twice before failing (I-3). Recursion children inherit `force`, so
their type conflicts follow it (D-27, retired).

**`replace`.** `replace => false` is consulted where Puppet consults it
(`type/file/ensure.rb:162-165`, `target.rb:66`, `data_sync.rb:38`). Under it
`ensure` is in sync for anything that exists, whatever its type, whatever
`ensure` asks for and whatever `force` says: nothing at the path is removed,
replaced or written (`keptByReplaceFalse`, checked in `Apply` after the source
resolves).

So `ensure => absent` keeps a file, `ensure => file` keeps a
directory or a link, `ensure => directory` keeps a file, and a purge removes
nothing, because every purged child inherits `replace`. `content`, `source`
content and `target` are in sync too, while `mode`, `owner`, `group` and the
SELinux properties still reconcile as ordinary properties. All of it was
measured on Puppet 8.10.0.

The path is read as `links` says, so under `follow`
a dangling link counts as absent. A missing path is created as usual.

**`show_diff`** is accepted and has no effect: Puppet prints a diff only when
the global `show_diff` setting is on, and voxagent has no such setting. The
`augeas` type's `show_diff` is the same (`provider/augeas/augeas.rb:466`).

G2 (`tests/spine/idempotence.sh`) holds the cases that converge. The ones it
leaves out cannot: `keepdir` (I-1), `ensure => file` with a `target` over an
existing path (I-6), and `follow` over an existing link under
`ensure => link`, which Puppet re-creates every run.

## `file` SELinux contexts

`seluser`, `selrole`, `seltype`, `selrange` and `selinux_ignore_defaults` are
implemented in `internal/units/file/file_selinux.go`, which cites Puppet's and
libselinux's source line by line. This section says how the pieces fit.
Puppet's side was first read, not measured, because no host or container here
makes Puppet see SELinux as enabled; [the SELinux gate](gate-evidence.md#the-selinux-gate) has since
measured it on an enforcing VM, where every resulting label agrees.

**Is SELinux enabled?** As libselinux 3.11's `is_selinux_enabled` is
(`enabled.c:11-21`, `init.c:31-152`): a **read-write** selinuxfs mount **and**
`/etc/selinux/config` existing. A container that sees the host's selinuxfs
read-only therefore reads as disabled. It is computed once per process, as
libselinux does in its constructor. When SELinux is disabled every `sel*`
property is a no-op: no lookup, no read, no write.

**Which paths are managed.** Only a path on one of ext2/3/4, gfs, gfs2, xfs,
jfs, btrfs, tmpfs or zfs, chosen by the deepest `/proc/mounts` entry that
prefixes it (`util/selinux.rb:220-226`); on any other filesystem the properties
are in sync. Mount points are not unescaped, as Puppet's are not (I-8).

**Defaults and the `selabel_lookup` dependency.** Unless
`selinux_ignore_defaults` is true, the default context comes from the host's
`selabel_lookup -b file -k <path> -t <mode>` (package `libselinux-utils`), so
matching is libselinux's own and there is no cgo. The mode is the path's
`lstat` mode before the run, or one derived from the declared `ensure` for a
path not there yet (I-10, I-11). A failed lookup is no default, as in Puppet.

When SELinux is enabled and a `File` wants defaults and can sync its SELinux
properties (a removal counts only where `replace => false` keeps the path;
`syncsSELinux`), `Validate` refuses the **whole catalog** before anything is
applied in two cases. The tool is not on `PATH`: the refusal names the
package, because a lookup that cannot run would otherwise leave the node
half-managed. Or the file-contexts handle does not open, on a filesystem that
carries labels: `File[<path>]: Cannot get default context with nil handle`,
because Puppet fails the whole run then too (D-35).

The handle is probed once
per run (`handleOpens`, a lookup of `/`); a lookup that still meets a nil
handle during the apply fails that resource. A declared value replaces the
default field by field.

**Reading and writing a label.** Both go through a handle opened with
`O_PATH|O_NOFOLLOW` by the file's own `fileSystem` (the `os.Root` for a
recursion child), using `getxattr`/`setxattr` on `/proc/self/fd/N`, as
`selinux_restorecon` does. That acts on the object opened, a link included, and
never resolves the name again, so a directory swapped for a link out of a
recursed tree cannot make the relabel land outside it.
`lgetxattr`/`lsetxattr` by path would follow such a swap. A walked symlink
child is relabelled itself through that handle, never its target (D-28,
retired).

**Order and silence.** SELinux is applied last in `applyMetadata`, after owner,
group and mode. A path voxagent creates, and a file whose content it rewrites,
is relabelled silently, with no event: that is Puppet's `property_fix` after
`ensure` and after a content write. A path that already exists and is out of
sync gets one `seltype_changed`-style event per property. A relabel that fails
is only a warning, `Failed to set SELinux context <ctx> on <path>`, and the
event still succeeds (I-4). A file with an unreadable label fails with
the text of Puppet's internal Ruby error (I-9), and `selrange` and a literal
`unlabeled` are edited as Puppet edits them (I-7, I-12). The agent does not
translate with mcstrans (D-33).

**What a lookup costs.** One `selabel_lookup` process per managed `File` that
wants defaults, except for recursion children, which are batched (below).

Measured 2026-10-06 in `localhost/fleet-node-el` (`libselinux-utils` and
`selinux-policy-targeted` installed) on an AMD Ryzen 9 5900X: a throwaway Go
benchmark calling `hostSELinux.defaultFor` 5,000 times in sequence for
`/etc/passwd`, three runs. The first run took 9.5 ms per call (cold file
cache), the next two 4.2 and 4.1 ms; 5,000 lookups are therefore about
**20 s** warm and about 47 s on the cold first run.

In
`performance.md`, Ruby applies the whole set of 5,000 `file` resources in 8.4 s
steady and 28.0 s cold. Compared like for like, per-path lookups alone exceed Ruby's whole apply
both warm (20 s against 8.4 s) and cold (47 s against 28.0 s).

**Top-level `File`s are not batched.** On an SELinux-enabled host each
top-level `File` that wants defaults runs its own `selabel_lookup`, about
4 ms warm, and each also re-reads `/proc/mounts` in `labelSupported` to ask
whether its filesystem carries labels, once in `Validate` (before the
once-per-run handle probe, D-35) and once in `Apply`. Only recursion children share a batch
(below). The 146 top-level `File`s in the SIMP catalog cost about 0.6 s in lookups.

The measurement has limits. It looked up one path (`/etc/passwd`)
sequentially, so a policy search over many different paths may cost more or
less per call. Ruby's figures are for a content+mode catalog on a host without
SELinux, not a recursion and not SELinux-enabled, so they are not a like-for-like
total. The lookup time against Ruby's whole apply is therefore a lower bound on
voxagent's extra cost on an SELinux-enabled host, not the difference between
the two agents.

**Batched lookups for recursion children.** A recursed `File`'s `Generate`
(on the provider `Prefetch` returned for the run) looks up its children's
defaults through `matchpathcon -n -m <type> -- <paths...>`, one process per
file type and per 64 KiB of arguments, and each child's `Apply` takes its
answer instead of running `selabel_lookup` (`prefetchChildDefaults`,
`hostSELinux.defaultsFor`). An answer is used only for the exact path and mode
the child's `Apply` asks for, so a child whose type changed in between, or one
the batch could not answer, is looked up per path as before. Not batched:

- a child whose lookup mode has no file type (an absent path with no declared
  `ensure`, I-11, so most children of a `source` copied into a new tree):
  `matchpathcon` cannot force mode 0, and without `-m` it would `lstat` the
  path itself;
- output that does not map back line for line (a lookup error, a warning on
  stderr) or a `matchpathcon` that cannot run: that chunk goes per path;
- top-level `File`s.

`matchpathcon` gives `selabel_lookup`'s answers: it opens the same handle (file
backend, no path, no validation) and calls `selabel_lookup` the same non-raw way,
with the bare `S_IF*` bits, which are all the backend compares
(libselinux 3.11 `utils/matchpathcon.c:27-44, 50-71, 134-152`,
`src/label_file.c:2099-2122`). Checked 2026-10-06 in the same container:
1,214 paths (the image's `/etc`, `/usr/bin`, `/var`, `/run`, `/home`, `/root`
trees plus hand-picked ones including `/etc/shadow`, `/tmp`, `/usr/tmp`,
`/home/u/.ssh/authorized_keys`, a missing path, a path with a space, a tab
and a non-ASCII byte) by 10 modes (each file type, with and without permission
bits) gave 12,140 identical answers, single and batched. A `selabel_open`
failure is still `Cannot get default context with nil handle` for every child.

Measured the same way, 5,000 distinct paths through the batch took 28-39 ms
(three runs), against 21.0 s per path in the same container. Asking whether
the parent's filesystem supports labels is done once per recursion; each
child's `Apply` still asks for itself, which reads `/proc/mounts` each time,
about 0.2 ms a call or 1.1 s per 5,000 children.
