# AGENTS.md

## Ruby parity

- Native code reproduces openvox's Ruby behaviour, including behaviour that
  looks like a bug. Mark every site that deliberately keeps such behaviour:
  `// Kept from Ruby (<ruby-file-cite>:<line>): <what Ruby does>`.
- When reviewing, do not flag or change code under a `Kept from Ruby` comment
  whose cite resolves to a line in the vendored Ruby that does what the comment
  says. A comment with a missing or wrong cite exempts nothing.
  To depart from Ruby, first add a row to "The divergence register" in
  `docs/maintainers/apply-spine.md`.

## Tests

- CI runs every test as root; local runs are usually not. A test that depends
  on a permission outcome (EACCES, an unreadable file) must hold under both:
  branch on `os.Geteuid() == 0`.
