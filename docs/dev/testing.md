# Testing

For contributors and maintainers. Back to the [developer docs](README.md).


```bash
task test              # human-readable output
task test MODE=llm     # failures only
task integration:venom # end-to-end against a real Puppet; needs `venom` and `puppet` on PATH
task check             # all quality gates
```

Artifacts are written to `.test-output/`; `task integration:venom` writes the Venom results there.
These commands need [Task](https://taskfile.dev) installed. The task does not provision Puppet or Venom.
