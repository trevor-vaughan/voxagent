//go:build unix

package ral

import "time"

// cancelGrace bounds how long runExec waits for the leader after the context is
// cancelled and the process group has been killed.
//
// Killing the group is what actually stops the work; this is the second line of
// defence for a shell that somehow survives the signal. Exec's output is a
// temporary file rather than a pipe, so no descendant can hold Wait open.
const cancelGrace = time.Second
