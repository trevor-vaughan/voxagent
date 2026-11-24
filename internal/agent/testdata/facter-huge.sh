#!/bin/sh
# Fake facter: emits more output than the cap allows.
printf '{"blob":"'
head -c 70000000 /dev/zero | tr '\0' a
printf '"}'
