# Native units: `voxagent units`

For operators. Back to the [usage index](README.md).


Every Puppet type voxagent applies itself is implemented by a unit. `voxagent units`
prints one JSON array holding every unit's manifest, sorted by name: its version,
the Ruby implementation it reproduces (`source`) and the one it was tested against
(`oracle`), the types it serves, the units it requires, the platforms its parity
was measured on, and, for module units, the file digests of each measured release. A core unit reproduces one of Puppet's built-in
types (`source.implementation` is `puppet`); a module unit reproduces a Puppet module (`source.implementation` is the
module's name). A module's type is native only when the plugin tree under `--libdir` (the option `voxagent apply` and
`voxagent agent` take, described in [What voxagent applies](resource-types.md)) matches one of the releases.

Build the binary first with `task build`, which writes `bin/voxagent`.

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
  "platforms": [
    {
      "id": "*",
      "like": false,
      "min_major": 0
    }
  ],
  "releases": []
}
```

Each `platforms` entry has three fields:

- `id`: the `ID` in the host's os-release. `*` means any platform, and no os-release is read.
- `like`: when `true`, a host whose `ID_LIKE` names `id` also matches.
- `min_major`: the lowest major of `VERSION_ID` accepted; `0` means any.

A unit is native on a host only when some entry matches. For example, `{"id": "rhel", "like": true, "min_major": 10}`
covers RHEL and its derivatives at version 10 or later, and not Fedora.

A module unit's element differs in `source` (module name, empty `version`), `requires`, `platforms` and `releases`.
`jq '.[] | select(.name == "host_core")'` prints, abridged to the fields that differ:

```json
{
  "name": "host_core",
  "source": { "implementation": "puppetlabs-host_core", "version": "" },
  "requires": ["group", "file"],
  "releases": [
    {
      "version": "1.3.0",
      "files": [
        { "path": "puppet/type/host.rb", "sha256": "e0d448a5835a12a2565d309b348ed071c0f849aeec87372777c948acc6bf1e35" },
        { "path": "puppet/provider/host/parsed.rb", "sha256": "a396f906038d718f6c69cb7a836f13eed16de5e9b2cf9d39a020eed9e9685f7d" }
      ],
      "types": {
        "host": ["puppet/type/host.rb", "puppet/provider/host/parsed.rb"]
      }
    }
  ]
}
```

Each release holds:

- `version`: the module version measured.
- `files`: each file's `path` under the plugin tree and its lowercase hex `sha256`.
- `types`: for each type the unit serves, the files that must all match their digests for that type to be native. An entry
  `<unit>:<path>` names a file listed by a required unit.

The command takes no arguments: one prints a usage line on stderr and exits 2.
Otherwise it exits 0. It reads no file, so its output does not depend on the host,
a catalog or `--libdir`.
