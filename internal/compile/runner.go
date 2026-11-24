// Package compile invokes a Puppet implementation and captures the catalog it
// produces. It does no parsing; that is internal/catalog's job.
package compile

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// Runner invokes one Puppet implementation. Every field maps to a CLI flag, so
// two Runners pointed at different binaries or environments can be compared
// directly. When the Go agent gains a compile path, it becomes a second Runner
// with a different PuppetBin.
type Runner struct {
	PuppetBin       string
	EnvironmentPath string
	Environment     string
	ConfDir         string
	VarDir          string
	CodeDir         string
}

// Compile produces the catalog JSON for node.
//
// Puppet writes a "Notice: Compiled catalog ..." line to stdout ahead of the
// JSON body, including ANSI escapes, so the output is trimmed to the JSON
// object before returning.
func (r Runner) Compile(ctx context.Context, node string) ([]byte, error) {
	args := []string{
		"catalog", "find", node,
		"--terminus", "compiler",
		"--render-as", "json",
		"--node_name_value", node,
	}
	if r.EnvironmentPath != "" {
		args = append(args, "--environmentpath", r.EnvironmentPath)
	}
	if r.Environment != "" {
		args = append(args, "--environment", r.Environment)
	}
	if r.ConfDir != "" {
		args = append(args, "--confdir", r.ConfDir)
	}
	if r.VarDir != "" {
		args = append(args, "--vardir", r.VarDir)
	}
	if r.CodeDir != "" {
		args = append(args, "--codedir", r.CodeDir)
	}

	cmd := exec.CommandContext(ctx, r.PuppetBin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("compile catalog for %s: %w: %s", node, err, stderr.String())
	}
	return ExtractJSON(stdout.Bytes())
}

// ExtractJSON trims everything before the first line that begins a JSON object.
//
// Scanning for the first line starting with '{' rather than the first '{'
// anywhere avoids being fooled by a brace inside the Notice text.
func ExtractJSON(out []byte) ([]byte, error) {
	for i := 0; i < len(out); {
		end := bytes.IndexByte(out[i:], '\n')
		var line []byte
		if end < 0 {
			line = out[i:]
		} else {
			line = out[i : i+end]
		}
		if len(bytes.TrimSpace(line)) > 0 && bytes.TrimSpace(line)[0] == '{' {
			return out[i+bytes.IndexByte(out[i:], '{'):], nil
		}
		if end < 0 {
			break
		}
		i += end + 1
	}
	return nil, fmt.Errorf("no JSON object found in compiler output: %.200q", out)
}
