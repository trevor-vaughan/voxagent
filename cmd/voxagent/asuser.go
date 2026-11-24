package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/voxagent/voxagent/internal/lib/asuser"
	"github.com/voxagent/voxagent/internal/units/file"
)

// asuserEuid is the helper's euid check; tests replace it.
var asuserEuid = os.Geteuid

// asuserCommand is the hidden "voxagent asuser" helper (spec §3.4), which the
// ssh_authorized_key flush starts as the key's user:
//
//	asuser mkdir <dir>
//	asuser write --encoding <enc> <path>   (the text on stdin)
//	asuser chmod <path>
//
// Its user may ptrace it, so its output is untrusted: it prints only the
// op's result line ("<step> <errno>") and exits 0 or 1 as asuser.ParseResult
// expects. It refuses euid 0 ("asuser: refusing to run as root", exit 2), and
// any other invocation, or text the encoding cannot take (which the parent
// checks before it starts the write), gets exit 2 and no output.
func asuserCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if asuserEuid() == 0 {
		fmt.Fprintln(stderr, "asuser: refusing to run as root")
		return 2
	}
	var r asuser.Result
	switch {
	case len(args) == 2 && args[0] == "mkdir":
		r = asuser.Mkdir(args[1])
	case len(args) == 2 && args[0] == "chmod":
		r = asuser.Chmod(args[1])
	case len(args) == 4 && args[0] == "write" && args[1] == "--encoding":
		text, err := io.ReadAll(stdin)
		if err != nil {
			return 2
		}
		if r, err = asuser.Write(context.Background(), args[3], args[2], string(text), file.Relabel); err != nil {
			return 2
		}
	default:
		return 2
	}
	if _, err := fmt.Fprintln(stdout, r); err != nil {
		return 2
	}
	return r.ExitCode()
}
