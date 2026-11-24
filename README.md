# voxagent

A Go Puppet agent with opportunistic Ruby fallback.

**Status:** proof of concept.

## What this is

voxagent applies a Puppet catalog: it reads the catalog from a file, refuses anything it cannot fully implement,
applies the rest, and writes a `report_format 12` report. The repository also contains `voxdiff`, which compares
catalogs and implementations (build it with `task build`; see [voxdiff](docs/usage/voxdiff.md)), and a fleet harness, a
containerised Puppet fleet that exercises both against realistic data (see the [fleet harness](docs/dev/fleet-harness.md)).

## Install

Build from source with Go, a C compiler and [Task](https://taskfile.dev) (the `task` command, which runs the
repository's Taskfile). `task build` writes `bin/voxagent`, `bin/voxdiff` and `bin/gatekit`:

```bash
task build
```

voxagent is built with cgo (`CGO_ENABLED=1`, Go's default where a C compiler
is installed): `user` and `group` read accounts through libc's name service,
as Puppet does, and `sysctl`, `ssh_config`, `sshd_config`,
`sshd_config_subsystem` and `augeas` load [OpenVox](https://voxpupuli.org/openvox/)'s libaugeas with `dlopen` (the build links
no Augeas library), so a `CGO_ENABLED=0` build fails.

## Quickstart

```bash
./bin/voxagent apply --catalog catalog.json --detailed-exitcodes
./bin/voxagent agent --server puppet --detailed-exitcodes   # one run against an OpenVox server
```

`catalog.json` is a compiled Puppet catalog in JSON that you supply; the repository does not ship one.
`puppet` in the second command is the server's host name; its certificate must name it. Enrolment, certificates and
the server prerequisites are in [Running against an OpenVox server](docs/usage/openvox-server.md).

`voxagent agent --help` lists its flags; their defaults are Puppet's compiled-in
settings (`puppet.conf` is not read).

## Where to go next

- **Using it:** [docs/usage/](docs/usage/README.md): supported resource types, log levels, backups, the Ruby extension
  host, running against an OpenVox server, and `voxdiff`.
- **Contributing:** [docs/dev/](docs/dev/README.md): testing, the gates, and maintainer notes on how the code works.
- **Security:** [SECURITY.md](SECURITY.md): how to report a vulnerability.
- **License:** [LICENSE.md](LICENSE.md). voxagent is a derivative work of
  [Puppet](https://github.com/puppetlabs/puppet), which is licensed the same way; the attribution is in
  [NOTICE](NOTICE).
