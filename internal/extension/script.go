package extension

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed host.rb
var hostScript []byte

// HostScriptSHA256 is the SHA-256 of host.rb as reviewed. The staged copy is
// checked against it before exec, and TestHostScriptHashIsPinned fails when
// host.rb changes without it.
const HostScriptSHA256 = "8c2f1a3ae4a1680e55cce754f16a486143dc7a94e2b7a5eab4fc12dac051ac62"

// Config is how voxagent runs the host.
type Config struct {
	// Ruby is the interpreter's absolute path. It is configuration and never
	// looked up on PATH.
	Ruby     string
	Timeouts Timeouts
}

// Launch stages the embedded host and starts it with the host environment
// filtered from environ.
func Launch(cfg Config, environ []string) (*Host, error) {
	if !filepath.IsAbs(cfg.Ruby) {
		return nil, fmt.Errorf("the Ruby path %q is not absolute", cfg.Ruby)
	}
	dir, script, err := stageScript(hostScript, HostScriptSHA256)
	if err != nil {
		return nil, err
	}
	h, err := Start([]string{cfg.Ruby, script, filepath.Join(dir, "state")}, HostEnv(environ), dir, cfg.Timeouts)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return h, nil
}

// stageScript writes script into a fresh 0700 directory, with a 0700 state
// directory beside it, and verifies the written bytes against want. On any
// failure the directory is removed.
func stageScript(script []byte, want string) (dir, path string, err error) {
	staged, err := os.MkdirTemp("", "voxagent-host-")
	if err != nil {
		return "", "", fmt.Errorf("stage the extension host: %w", err)
	}
	// Removes staged, not the named result dir: `return "", "", err` blanks dir
	// before this deferred call runs.
	defer func() {
		if err != nil {
			_ = os.RemoveAll(staged)
		}
	}()
	dir = staged
	path = filepath.Join(dir, "host.rb")
	if err = os.WriteFile(path, script, 0o600); err != nil {
		return "", "", fmt.Errorf("stage the extension host: %w", err)
	}
	if err = os.Mkdir(filepath.Join(dir, "state"), 0o700); err != nil {
		return "", "", fmt.Errorf("stage the extension host: %w", err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("stage the extension host: %w", err)
	}
	sum := sha256.Sum256(written)
	if got := hex.EncodeToString(sum[:]); got != want {
		err = fmt.Errorf("the staged extension host %s does not match the hash compiled into voxagent", got)
		return "", "", err
	}
	return dir, path, nil
}
