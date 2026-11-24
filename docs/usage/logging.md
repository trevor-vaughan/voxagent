# Log levels: `--verbose` and `--debug`

For operators. Back to the [usage index](README.md).


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
| `--debug` | also `Debug:` lines: Puppet's debug lines for the graph, the transaction and the resource types (for example `exec`, `file`, `package`, `service`, `user` and `group`), and voxagent's own, which name no parameter value, file content, header, certificate, key or HTTP body |

`Sensitive` values are redacted from debug lines as from error text: a marked
command shows as `[redacted]` and its output as `[output redacted]`. A run
without `--debug` writes the same report as a run did before debug lines
existed. Of Puppet's debug lines voxagent emits a subset, and the register of
the ones it omits is row D-82 in
[the apply-spine decisions](../maintainers/apply-spine.md). How the run logger works is in
[the run logger notes](../maintainers/runlog.md).

Two caps bound a run's memory. A command's output is kept up to 64 MiB, cut at
a line break, or at the last complete UTF-8 sequence when it has none; past it
voxagent logs the warning `<resource>: output of <command> truncated at 64 MiB`
and carries on. The report keeps at most 16 MiB of debug
text; after that it records the warning `debug log truncated in the report
after 16 MiB; the console has the rest` and the console alone shows further
debug lines. Both are fixed, not flags.
