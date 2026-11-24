// Package agent runs one Puppet agent run against an OpenVox server.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/voxagent/voxagent/internal/puppetapi"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
)

const (
	factsTimeout = 5 * time.Minute
	// factsMaxOutput bounds facter's stdout.
	factsMaxOutput = 64 << 20
	// factsMaxStderr bounds the facter stderr kept for error messages.
	factsMaxStderr = 64 << 10
	// factsExpiry is how long the server may treat the facts as fresh.
	factsExpiry = 30 * time.Minute
	// factsTimeFormat matches the sample capture: nanosecond precision and a
	// numeric "+00:00" offset (never "Z").
	factsTimeFormat = "2006-01-02T15:04:05.000000000-07:00"
)

// FactsConfig configures Facts. Facter must be an absolute path. Now defaults
// to time.Now.
type FactsConfig struct {
	Facter, Certname, LibDir, PluginFactDest string
	Noop                                     bool
	Now                                      func() time.Time
}

// capWriter keeps at most max bytes and fails the write once exceeded, which
// makes exec close the pipe and the child die of EPIPE.
type capWriter struct {
	buf      bytes.Buffer
	max      int
	exceeded bool
}

var errOutputCap = errors.New("output exceeds cap")

func (w *capWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.max {
		w.exceeded = true
		return 0, errOutputCap
	}
	return w.buf.Write(p)
}

// truncWriter keeps the first max bytes and silently drops the rest, so a
// noisy stderr never fails a run.
type truncWriter struct {
	buf bytes.Buffer
	max int
}

func (w *truncWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// Facts runs facter and returns the catalog request's JSON wrapper:
// {"name","values","timestamp","expiration"} (expiration = timestamp + 30m).
//
// Measured against localhost/fleet-node-el (openvox-agent) on 2026-11-16:
// `facter --help` offers --json, --custom-dir, --external-dir and
// --show-legacy; legacy facts are off by default. `facter --json` yields the
// sample's values minus implementation, clientcert, clientversion, clientnoop
// (added here) and minus puppetversion, which only `facter --puppet` loads.
// We do not pass --puppet (it would load Puppet's settings into this root
// process); puppetversion is added with the same value as clientversion. The
// only other difference is the external fact vox_probe_external, which
// appears once --external-dir points at the plugin facts directory.
//
// Hardening: relative facter paths are refused; facter
// runs with an environment built from scratch (PATH, LANG, HOME only), stdin
// /dev/null, capped output and a timeout. Fact values are never logged; on
// failure facter's stderr (truncated to 64 KiB) is included in the error.
func Facts(ctx context.Context, c FactsConfig) ([]byte, error) {
	if !filepath.IsAbs(c.Facter) {
		return nil, fmt.Errorf("facter path %q must be absolute", c.Facter)
	}
	now := c.Now
	if now == nil {
		now = time.Now
	}

	args := []string{"--json"}
	if c.LibDir != "" {
		args = appendIfDir(args, "--custom-dir", filepath.Join(c.LibDir, "facter"))
	}
	args = appendIfDir(args, "--external-dir", c.PluginFactDest)

	ctx, cancel := context.WithTimeout(ctx, factsTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Facter, args...)
	cmd.Env = []string{
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG=C",
		"HOME=" + os.Getenv("HOME"),
	}
	cmd.Stdin = nil // exec connects the null device
	stdout := &capWriter{max: factsMaxOutput}
	stderr := &truncWriter{max: factsMaxStderr}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = 5 * time.Second
	started := time.Now()
	err := cmd.Run()
	runlog.Debugf(ctx, "voxagent", "facter %s in %s", strings.Join(args, " "), time.Since(started).Round(time.Millisecond))
	if err != nil {
		// The child usually dies of SIGPIPE, so Run reports an exit status
		// rather than the writer's error; check the writer itself.
		if stdout.exceeded {
			return nil, fmt.Errorf("facter output exceeds %d bytes", factsMaxOutput)
		}
		return nil, fmt.Errorf("facter failed: %w: %s", err, puppetapi.Sanitise(string(bytes.TrimSpace(stderr.buf.Bytes()))))
	}

	var values map[string]any
	if err := json.Unmarshal(stdout.buf.Bytes(), &values); err != nil {
		return nil, fmt.Errorf("facter output is not a JSON object: %w", err)
	}
	if values == nil {
		return nil, errors.New("facter output is not a JSON object: null")
	}
	values["implementation"] = "openvox"
	values["clientcert"] = c.Certname
	values["clientversion"] = report.PuppetVersion
	values["puppetversion"] = report.PuppetVersion
	values["clientnoop"] = c.Noop

	ts := now() // local zone, as Puppet's Time.now
	return json.Marshal(struct {
		Name       string         `json:"name"`
		Values     map[string]any `json:"values"`
		Timestamp  string         `json:"timestamp"`
		Expiration string         `json:"expiration"`
	}{c.Certname, values, ts.Format(factsTimeFormat), ts.Add(factsExpiry).Format(factsTimeFormat)})
}

// appendIfDir adds "flag dir" to args when dir is an existing directory.
func appendIfDir(args []string, flag, dir string) []string {
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return append(args, flag, dir)
	}
	return args
}
