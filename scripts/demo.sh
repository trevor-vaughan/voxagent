#!/usr/bin/env bash
# Render the README hero demo (scripts/demo.tape) into a GIF with VHS.
#
# The recording runs against a throwaway directory under $(mktemp -d), never the
# working tree: the catalog manages files under that directory, so the demo is
# offline, needs no root, and leaves nothing behind. voxagent is built fresh into
# the sandbox so the recorded shell runs the current source, not a stale bin/.
#
# Usage: demo.sh <root_dir> <tape> <output_gif>
#   root_dir     repository root (used to locate ./cmd/voxagent)
#   tape         path to the .tape file to render
#   output_gif   path to write the finished GIF to
#
# Requires on PATH: go, vhs, ttyd, ffmpeg, jq (the tape uses jq on screen).
set -euo pipefail

if [[ $# -ne 3 ]]; then
	echo "usage: demo.sh <root_dir> <tape> <output_gif>" >&2
	exit 2
fi

ROOT_DIR=$1
TAPE=$2
OUTPUT_GIF=$3

for tool in go vhs ttyd ffmpeg jq; do
	command -v "$tool" >/dev/null 2>&1 || {
		echo "demo.sh: '$tool' not found on PATH" >&2
		exit 1
	}
done
[[ -f "$TAPE" ]] || {
	echo "demo.sh: tape not found: $TAPE" >&2
	exit 1
}

# A short template keeps the absolute resource titles on screen readable.
WORK=$(mktemp -d "${TMPDIR:-/tmp}/vox.XXXXXX")
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

mkdir -p "$WORK/bin" "$WORK/srv"
(cd "$ROOT_DIR" && go build -o "$WORK/bin/voxagent" ./cmd/voxagent)

# The catalog voxagent converges: a directory, a file with managed content, and
# a creates-guarded exec. No notify and no unguarded exec -- either reports a
# change on every run, and the demo's second run must show exit 0 (the same
# constraint tests/spine/idempotence.sh documents).
cat >"$WORK/catalog.json" <<EOF
{"catalog_format":2,"name":"web01.example.com","environment":"production",
 "version":1,"catalog_uuid":"00000000-0000-4000-8000-000000000001",
 "tags":[],"classes":[],"edges":[],
 "resources":[
  {"type":"File","title":"$WORK/srv/app","tags":[],
   "parameters":{"ensure":"directory","mode":"0755"}},
  {"type":"File","title":"$WORK/srv/app/motd","tags":[],
   "parameters":{"ensure":"file","content":"managed by voxagent\n","mode":"0644"}},
  {"type":"Exec","title":"seed-cache","tags":[],
   "parameters":{"command":"/usr/bin/touch $WORK/srv/app/.seeded",
                 "creates":"$WORK/srv/app/.seeded"}}
 ]}
EOF

# A catalog holding types voxagent has no native unit for. With --no-ruby it
# must refuse the whole catalog before applying anything, File included.
cat >"$WORK/nginx.json" <<EOF
{"catalog_format":2,"name":"web01.example.com","environment":"production",
 "version":2,"catalog_uuid":"00000000-0000-4000-8000-000000000002",
 "tags":[],"classes":[],"edges":[],
 "resources":[
  {"type":"File","title":"$WORK/srv/app/motd","tags":[],
   "parameters":{"ensure":"file","content":"managed by voxagent\n"}},
  {"type":"Nginx::Vhost","title":"shop","tags":[],"parameters":{"ensure":"present"}},
  {"type":"Mysql_user","title":"app@localhost","tags":[],"parameters":{"ensure":"present"}}
 ]}
EOF

# The tape's hidden preamble reads $VOX_DEMO_DIR to cd into the sandbox; -o
# overrides the tape's placeholder Output path.
mkdir -p "$(dirname "$OUTPUT_GIF")"
PATH="$WORK/bin:$PATH" VOX_DEMO_DIR="$WORK" vhs -o "$OUTPUT_GIF" "$TAPE"

echo "demo.sh: wrote $OUTPUT_GIF"
