package augeasproviderssysctl

import (
	"context"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/augeas"
	"github.com/voxagent/voxagent/internal/lib/command"
	"github.com/voxagent/voxagent/internal/ral"
	core "github.com/voxagent/voxagent/internal/units/augeasproviders_core"
)

// augeasproviders_sysctl's sysctl type. Its Apply, Prefetch and Refresh come
// with the run contracts (spec §5, §6), in sysctl_apply.go and
// sysctl_prefetch.go; its attributes and validation are spec §3.

// sysctlRunner runs the sysctl command and returns its combined output.
type sysctlRunner interface {
	run(ctx context.Context, name string, args ...string) (int, string, error)
}

// Sysctl is the native augeas provider for augeasproviders_sysctl's sysctl.
type Sysctl struct {
	run *core.Run
	// sysctl is the command's path, resolved when the type is routed.
	sysctl string
	runner sysctlRunner
}

// argvSysctl runs sysctl as Puppet's optional_commands do: runArgvWith, so
// Ruby's locale override, combined output and the Executing line.
type argvSysctl struct{}

func (argvSysctl) run(ctx context.Context, name string, args ...string) (int, string, error) {
	return command.RunArgvWith(ctx, command.ArgvOpts{}, name, args...)
}

// EndRun implements ral.RunEnding: the type ends the run it shares with the
// other augeasproviders types, which closes every handle once however many of
// them call it.
func (s Sysctl) EndRun() { s.run.EndRun() }

func (Sysctl) Type() string { return "sysctl" }

func (Sysctl) ProviderUsed() *string { return &augeas.ProviderName }

// Attrs is every attribute the type declares, name and provider included
// (spec §3's allattrs).
func (Sysctl) Attrs() ral.Attrs {
	return ral.NewAttrs("name", "provider", "ensure", "val", "value", "comment", "target", "apply", "persist", "silent")
}

// Validate implements Validating: the first failure in allattrs order, which
// is Ruby's. A value native does not implement is an *rubyvalue.UnimplementedValue.
func (Sysctl) Validate(r catalog.Resource) error {
	_, err := parseSysctl(r)
	return err
}

// Validation refused an unparseable resource before the graph is built, so a
// parse error here means no edge.

func (Sysctl) AutoRequires(r catalog.Resource, managed func(string) bool) []string {
	p, err := parseSysctl(r)
	if err != nil {
		return nil
	}
	return core.TargetRequire(p.Target, managed)
}
