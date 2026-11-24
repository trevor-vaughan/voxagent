// Package fleet holds the checks the fleet gates make on what the fleet
// produced: compiled catalogs, captured facts, OpenVoxDB's exported resources,
// and the manifest a negative control mutates. Each was a Python heredoc in a
// gate script; each reaches the verdict that heredoc did.
package fleet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/voxagent/voxagent/internal/compile"
	"github.com/voxagent/voxagent/internal/gatekit/pycompat"
)

// LoadCatalog reads the output `puppet catalog find` wrote to path and decodes
// the catalog in it.
//
// The body is found by compile.ExtractJSON -- the code voxdiff's own compile
// path uses, not a copy of it -- so a Notice line ahead of the JSON, ANSI
// escapes included, is skipped the same way in both places. This is not the
// judged code: the fleet gates check catalogs Puppet compiled.
//
// Invalid UTF-8 is deliberately NOT refused, unlike the report comparator's
// input. encoding/json replaces a corrupt byte inside a string with U+FFFD,
// which the Python reader this replaced also did, so a corrupted value reads
// as a different value -- a mismatch, a finding about the fleet -- while a
// corrupt byte in the JSON structure is a read failure.
func LoadCatalog(path string) (map[string]any, error) {
	out, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	body, err := compile.ExtractJSON(out)
	if err != nil {
		return nil, fmt.Errorf("no JSON object in the catalog output at %s", path)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%s: extra data after the catalog object", path)
	}
	catalog, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s holds %s, not a catalog object", path, pycompat.Shape(v))
	}
	return catalog, nil
}

// resources returns the catalog's resources, each an object. Python iterated
// whatever `resources` held and called .get on each element, so anything else
// crashed it; here it is a read failure.
func resources(catalog map[string]any) ([]map[string]any, error) {
	raw, present := catalog["resources"]
	if !present {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		if n, err := pycompat.Len(raw); err == nil && n == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("the catalog's resources are %s, not a list", pycompat.Shape(raw))
	}
	out := make([]map[string]any, len(list))
	for i, r := range list {
		resource, ok := r.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("the catalog's resources.%d is %s, not an object", i, pycompat.Shape(r))
		}
		out[i] = resource
	}
	return out, nil
}

// parameters is Python's `resource.get("parameters") or {}`: a falsy value is
// no parameters, and a truthy one that is not an object crashed the caller the
// moment it read a parameter -- so it is checked only where one is read.
func parameters(resource map[string]any) (map[string]any, error) {
	raw := resource["parameters"]
	if !pycompat.Truthy(raw) {
		return map[string]any{}, nil
	}
	params, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("a resource's parameters are %s, not an object", pycompat.Shape(raw))
	}
	return params, nil
}

// AxesWant is what one node's catalog must carry for its hiera axes to be
// intact: the fleet-message Notify's message, the tier file's content, and
// the comma-joined sorted titles of its fleet-pkg-* Notifys.
type AxesWant struct {
	Message, Tier, Packages string
}

// Axes returns one line per axis whose value in catalog differs from want, in
// the order fleet-message, fleet-tier, fleet-pkg-*. No lines means every axis
// holds. An error means the catalog could not be read as one.
//
// When a resource matches more than once the last one wins, as it did in the
// Python check, which assigned without breaking.
func Axes(catalog map[string]any, want AxesWant) ([]string, error) {
	list, err := resources(catalog)
	if err != nil {
		return nil, err
	}
	var message, tier any // nil until found: Python's None
	var packages []string
	for _, resource := range list {
		rtype, _ := resource["type"].(string)
		// Python matched on str(title). Only a string can equal or begin with
		// the titles below, so any other title matches nothing.
		title, _ := resource["title"].(string)
		switch {
		case rtype == "Notify" && title == "fleet-message":
			params, err := parameters(resource)
			if err != nil {
				return nil, err
			}
			message = params["message"]
		// Prefix-matched, so node-el's @@notify export is excluded by
		// construction: its title carries the variant gate check 4 mutates,
		// and matching it here would make this check fail whenever check 4
		// had run.
		case rtype == "Notify" && strings.HasPrefix(title, "fleet-pkg-"):
			packages = append(packages, title)
		case rtype == "File" && title == "/var/tmp/fleet-tier":
			params, err := parameters(resource)
			if err != nil {
				return nil, err
			}
			content := params["content"]
			if !pycompat.Truthy(content) {
				content = ""
			}
			s, ok := content.(string)
			if !ok {
				return nil, fmt.Errorf("/var/tmp/fleet-tier's content is %s, not a string", pycompat.Shape(content))
			}
			tier = pycompat.Strip(s)
		}
	}
	sort.Strings(packages)

	var lines []string
	for _, axis := range []struct {
		key        string
		found      any
		wantString string
	}{
		{"fleet-message", message, want.Message},
		{"fleet-tier", tier, want.Tier},
		{"fleet-pkg-*", strings.Join(packages, ","), want.Packages},
	} {
		if !pycompat.Equal(axis.found, axis.wantString) {
			lines = append(lines, fmt.Sprintf("    %s: expected %s, got %s",
				axis.key, pycompat.Render(axis.wantString), pycompat.Render(axis.found)))
		}
	}
	return lines, nil
}

// ComplianceOutcome is the verdict of the compliance check.
type ComplianceOutcome int

const (
	// Enforced: the marker carries the expected enforcement.
	Enforced ComplianceOutcome = iota
	// NotEnforced: the marker is present and says something else.
	NotEnforced
	// MarkerAbsent: fleet_profile declares the marker unconditionally, so its
	// absence means the catalog is not the one the check was written
	// against -- could not run, not a finding.
	MarkerAbsent
)

// Compliance reads the first Notify[fleet-compliance]'s message from catalog
// and compares it with expected. It returns the outcome and the lines to
// print; an error means the catalog could not be read as one.
func Compliance(catalog map[string]any, expected string) (ComplianceOutcome, []string, error) {
	list, err := resources(catalog)
	if err != nil {
		return 0, nil, err
	}
	var found any
	for _, resource := range list {
		rtype, _ := resource["type"].(string)
		title, _ := resource["title"].(string)
		if rtype == "Notify" && title == "fleet-compliance" {
			params, err := parameters(resource)
			if err != nil {
				return 0, nil, err
			}
			found = params["message"]
			break
		}
	}

	if found == nil {
		return MarkerAbsent, []string{"    Notify[fleet-compliance] is absent from the catalog"}, nil
	}
	if !pycompat.Equal(found, expected) {
		lines := []string{fmt.Sprintf("    fleet_profile::compliance_marker: expected %s, got %s",
			pycompat.Render(expected), pycompat.Render(found))}
		if pycompat.Equal(found, "unenforced") {
			lines = append(lines,
				"    the parameter fell back to its default: the enforcement backend",
				"    ran but no compliance check for it was loaded. Look at",
				"    compliance_markup::enforcement in hieradata/common.yaml and at",
				"    fleet_profile/SIMP/compliance_profiles/checks.yaml being on the",
				"    modulepath the profile compiler globs.")
		}
		return NotEnforced, lines, nil
	}
	return Enforced, []string{"    fleet_profile::compliance_marker = " + expected}, nil
}
