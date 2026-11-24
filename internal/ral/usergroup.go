package ral

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
)

// ConversionDebugging is implemented by a provider whose type logs debug lines
// while Puppet converts the catalog: the user and group types drop an
// attribute whose provider feature is missing and say so (type.rb:788-791).
// applyCatalog logs each line at source "/"+r.Ref(), resources in catalog
// order, after validation and before "Applying configuration".
type ConversionDebugging interface {
	ConversionDebug(r catalog.Resource) []string
}

// featureMissing is type.rb:790's debug line for a dropped attribute.
const featureMissing = "Provider %s does not support features %s; not managing attribute %s"

// unimplementedValue is a value shape the native user or group parser does
// not implement, though Ruby accepts it. NativeUserGroup sends a type with one
// to the extension host (spec §0); Ruby's own validation failures are other
// errors and stay native. shown is the value as the refusal prints it.
type unimplementedValue struct{ ref, attr, shown string }

func (e *unimplementedValue) Error() string {
	return fmt.Sprintf("%s: %s => %s is not implemented", e.ref, e.attr, e.shown)
}

// rubyBool is Puppet::Parameter::Boolean's munge (Puppet::Coercion.boolean):
// true, false, or a string that downcases to true/yes/false/no.
func rubyBool(v any) (bool, error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		switch strings.ToLower(t) {
		case "true", "yes":
			return true, nil
		case "false", "no":
			return false, nil
		}
	}
	return false, errors.New("expected a boolean value")
}

// shadowPath is the search order of Ruby's default PATH, where Puppet's
// command() finds the shadow-utils commands; tests point it elsewhere.
// shadowDirs is its colon-separated source, a string so that a test building
// the voxagent binary can link a directory of fake commands in its place
// (-ldflags -X, cmd/voxagent's TestNoSensitiveMarkerLeaks).
var (
	shadowDirs = "/usr/sbin:/usr/bin:/sbin:/bin"
	shadowPath = strings.Split(shadowDirs, ":")
)

// shadowCommand is Puppet::Util.which over shadowPath: the absolute path of
// the first executable regular file called name.
func shadowCommand(name string) (string, error) {
	for _, dir := range shadowPath {
		path := filepath.Join(dir, name)
		if (realPlatform{}).Executable(path) {
			return path, nil
		}
	}
	return "", fmt.Errorf("%s not found in %s", name, strings.Join(shadowPath, ":"))
}
