#!/bin/bash
# Fake facter: reports its initial environment (as exec'd, before bash adds
# its own variables) as the fact "env", one KEY=VALUE per line.
printf '{"env":"'
tr '\0' '\n' < /proc/$$/environ | while IFS= read -r l; do printf '%s\\n' "$l"; done
printf '"}'
