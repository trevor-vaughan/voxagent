#!/bin/bash
# Regenerates a Ruby-generated corpus of the inifile unit (spec §8.1).
#
#   gen.sh <release> <corpus>   release: 6.5.0 | 6.6.0 | 6.6.1
#                               corpus:  a testdata/gen/<corpus>.rb, e.g. munge
#
# Runs the generator in the oracle image against testdata/<release>/lib and
# writes testdata/<corpus>-<release>.json (section: section-6.6.1-pr581.json). The `section` corpus runs on 6.6.1
# with PR #581's lib hunk (tests/spine/inifile/581-lib.diff) applied to a
# temporary copy of the lib, never to the committed one.
set -euo pipefail

release=${1:?usage: gen.sh <release> <corpus>}
corpus=${2:?usage: gen.sh <release> <corpus>}
image=${ORACLE_IMAGE:-localhost/fleet-node-el}
ruby=/opt/puppetlabs/puppet/bin/ruby

here=$(cd "$(dirname "$0")" && pwd)
testdata=$(dirname "$here")
repo=$(cd "$testdata/../../../.." && pwd)
lib=$testdata/$release/lib
[ -d "$lib" ] || { echo "gen.sh: no $lib" >&2; exit 2; }
[ -f "$here/$corpus.rb" ] || { echo "gen.sh: no $here/$corpus.rb" >&2; exit 2; }

scratch=$(mktemp -d "$repo/.test-output/gen.XXXXXX" 2>/dev/null || { mkdir -p "$repo/.test-output" && mktemp -d "$repo/.test-output/gen.XXXXXX"; })
trap 'rm -rf "$scratch"' EXIT

out=$testdata/$corpus-$release.json
if [ "$corpus" = section ]; then
	out=$testdata/$corpus-$release-pr581.json
	[ "$release" = 6.6.1 ] || { echo "gen.sh: the section corpus is generated on 6.6.1 only" >&2; exit 2; }
	cp -r "$lib" "$scratch/lib"
	# The diff's paths are lib/puppet/...; the copy's root is the libdir.
	patch -p2 -d "$scratch/lib" <"$repo/tests/spine/inifile/581-lib.diff" >&2
	lib=$scratch/lib
fi

podman run --rm \
	-v "$lib:/inifile-lib:ro,Z" -v "$here:/gen:ro,Z" \
	-e RUBYLIB=/inifile-lib \
	"$image" sh -c "[ \"\$(/opt/puppetlabs/bin/puppet --version)\" = 8.34.0 ] || { echo 'gen.sh: image puppet is not 8.34.0' >&2; exit 3; }; exec $ruby /gen/$corpus.rb" \
	>"$scratch/out.json"
mv "$scratch/out.json" "$out"
