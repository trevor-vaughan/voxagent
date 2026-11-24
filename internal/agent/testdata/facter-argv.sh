#!/bin/sh
# Fake facter: reports its argv as the fact "argv", one argument per line.
printf '{"argv":"'
for a in "$@"; do printf '%s\\n' "$a"; done
printf '"}'
