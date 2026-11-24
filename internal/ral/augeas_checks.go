package ral

import (
	"fmt"
	"slices"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/puppetpath"
)

// The checks the augeas units' routes share (augeas_core and the
// augeasproviders units): they are engine pieces because no unit may import
// another that does not require it.

// augeasEnvironment are the variables that send every type to the host
// when set and non-empty (spec §2 (c)): libaugeas reads the AUGEAS_* ones,
// and either LD_* one could make voxagent load other code than Ruby does.
var augeasEnvironment = []string{"AUGEAS_ROOT", "AUGEAS_LENS_LIB", "AUGEAS_DEBUG", "AUGEAS_DEBUG_DIR", "LD_PRELOAD", "LD_LIBRARY_PATH"}

// AugeasRunChecks is spec §2 (c), shared by the augeas units' routes: "" when no variable
// of augeasEnvironment is set in env and libdir is absolute and clean, else
// the first reason. It runs before the probe so dlopen never runs under an
// AUGEAS_* or LD_* override.
func AugeasRunChecks(env Environ, libdir string) string {
	for _, v := range augeasEnvironment {
		if env(v) != "" {
			return v + " is set"
		}
	}
	if !strings.HasPrefix(libdir, "/") || !puppetpath.Clean(libdir) {
		return fmt.Sprintf("--libdir %q is not absolute and clean", libdir)
	}
	return ""
}

// HeldAugeas reports whether cat holds a resource of type typ (lower case),
// by case mapping, not case folding: "ſysctl" is not a Sysctl.
func HeldAugeas(cat *catalog.Catalog, typ string) bool {
	return slices.ContainsFunc(cat.Resources, func(r catalog.Resource) bool { return strings.ToLower(r.Type) == typ })
}

// AugeasProviderDir is "" when typ's provider directory in t holds no .rb but
// augeas.rb, else the reason: a second provider changes the one Ruby picks.
func AugeasProviderDir(t PluginTree, typ string) string {
	dir := "puppet/provider/" + typ
	names, err := t.List(dir)
	if err != nil {
		return fmt.Sprintf("%s: %v", dir, err)
	}
	for _, n := range names {
		if strings.HasSuffix(n, ".rb") && n != "augeas.rb" {
			return fmt.Sprintf("%s holds %s besides augeas.rb", dir, n)
		}
	}
	return ""
}
