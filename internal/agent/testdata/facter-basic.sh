#!/bin/sh
# Fake facter: prints a fixed fact set.
printf '{"kernel":"Linux","os":{"family":"RedHat"},"memory":{"system":{"total_bytes":1024}}}'
