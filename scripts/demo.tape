# voxagent README hero demo. Rendered by `task dev:demo`; see scripts/demo.sh,
# which builds the sandbox and seeds catalog.json and nginx.json.
# The Output path is a placeholder -- demo.sh overrides it with `vhs -o`.
Output "hero.gif"

Require voxagent
Require jq

Set Shell "bash"
Set Theme "Catppuccin Mocha"
Set FontSize 16
Set Width 1400
Set Height 720
Set Padding 20
Set WindowBar Colorful
Set TypingSpeed 25ms

# --- hidden setup: land in the sandbox with a short prompt ---------------------
Hide
Type `export PS1='\[\e[38;5;170m\]$\[\e[0m\] '` Enter
Type `cd "$VOX_DEMO_DIR" && clear` Enter
Show

# --- scene 1: converge, then prove idempotence --------------------------------
# Repeated apply commands type at 8ms: the viewer has already read them once.
Type "# a compiled Puppet catalog: a directory, a managed file, a guarded exec" Enter
Sleep 400ms
Type "jq -c '.resources[] | [.type, .title]' catalog.json" Enter
Sleep 2.5s
Type "# first run converges the node: exit 2 = changed" Enter
Sleep 400ms
Type `voxagent apply --catalog catalog.json --detailed-exitcodes; echo "exit $?"` Enter
Sleep 4s
Type "# second run: nothing to do, exit 0" Enter
Sleep 300ms
Type@8ms `voxagent apply --catalog catalog.json --detailed-exitcodes; echo "exit $?"` Enter
Sleep 2s

# --- scene 2: drift is put back -------------------------------------------------
Hide
Type "clear" Enter
Show
Type "# someone hand-edits a managed file..." Enter
Sleep 300ms
Type `echo "hand edit" > srv/app/motd` Enter
Sleep 500ms
Type@8ms `voxagent apply --catalog catalog.json --detailed-exitcodes; echo "exit $?"` Enter
Sleep 3s

# --- scene 3: refuse what it cannot fully implement ----------------------------
Hide
Type "clear" Enter
Show
Type "# no native implementation for a type? refused up front, nothing applied" Enter
Sleep 400ms
Type `voxagent apply --catalog nginx.json --no-ruby --detailed-exitcodes; echo "exit $?"` Enter
Sleep 4.5s
