#!/bin/sh
printf 'facter: \033]0;pwned\007\033[31mred\n' >&2
exit 3
