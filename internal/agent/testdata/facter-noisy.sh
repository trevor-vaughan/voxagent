#!/bin/sh
# Fake facter: writes far more stderr than the kept limit, then succeeds.
head -c 200000 /dev/zero | tr '\0' w >&2
printf '{"kernel":"Linux"}'
