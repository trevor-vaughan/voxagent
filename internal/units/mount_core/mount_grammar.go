package mountcore

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/voxagent/voxagent/internal/lib/parsedfile"
)

// mp:<n> is lib/puppet/provider/mount/parsed.rb of puppetlabs-mount_core.

// The /etc/fstab grammar's patterns (mp:34-44). A grammar pattern sees one
// line, which holds no "\n", so RE2's text anchors equal Ruby's line anchors;
// \s is parsedfile.Space and \S parsedfile.NotSpace (spec 4.5).
var (
	commentText = regexp.MustCompile(`^` + parsedfile.Space + `*#`)               // mp:34
	blankText   = regexp.MustCompile(`^` + parsedfile.Space + `*$`)               // mp:36
	fieldToken  = regexp.MustCompile(parsedfile.NotSpace + `+`)                   // the (?>\S+) of mp:43
	fieldSplit  = regexp.MustCompile(parsedfile.Space + `+`)                      // mp:185
	octalSpace  = `\040`                                                          // mp:193, 202
	fstabFields = []string{"device", "name", "fstype", "options", "dump", "pass"} // mp:23-24
)

// incompleteMatcher is the `incomplete` type's pattern (mp:44),
// ^(?!(\s*(?>\S+)){2}): true for a line with fewer than two whitespace-separated
// tokens at its start. RE2 has no lookahead, so it counts the tokens.
type incompleteMatcher struct{}

// Match implements parsedfile.Matcher: no captures.
func (incompleteMatcher) Match(line string) ([]parsedfile.Capture, bool) {
	return nil, len(fieldToken.FindAllStringIndex(line, 2)) < 2
}

// mountClass is the mount provider `parsed` as a parsedfile.Class (mp:9-233).
// now is the clock of the header.
func mountClass(newFileType func(target string) (parsedfile.FileType, error), now func() time.Time) *parsedfile.Class {
	grammar, err := parsedfile.NewGrammar(
		parsedfile.LineType{Name: "comment", Text: true, Match: parsedfile.RE2(commentText)},
		parsedfile.LineType{Name: "blank", Text: true, Match: parsedfile.RE2(blankText)},
		parsedfile.LineType{Name: "incomplete", Text: true, Match: incompleteMatcher{}},
		parsedfile.LineType{
			Name: "parsed", Fields: fstabFields, Optional: []string{"fstype", "options", "dump", "pass"},
			Separator: fieldSplit, Joiner: "\t",
			PostParse: decodeSpaces, PreGen: mountPreGen,
		},
	)
	if err != nil {
		panic("mount grammar: " + err.Error()) // the definition is static
	}
	return &parsedfile.Class{
		Name:          "parsed",
		TypeName:      "mount",
		Grammar:       grammar,
		DefaultTarget: defaultTarget,
		FileType:      newFileType,
		Now:           now,
		PrefetchHook:  mountPrefetchHook,
	}
}

// decodeSpaces is the parsed line's post_parse (mp:198-206): `\040` is a space
// in device and name.
func decodeSpaces(r *parsedfile.Record) error {
	for _, f := range []string{"device", "name"} {
		if v := r.Fields[f]; v.Kind == parsedfile.Str {
			v.S = strings.ReplaceAll(v.S, octalSpace, " ")
			r.Fields[f] = v
		}
	}
	return nil
}

// mountPreGen is the parsed line's pre_gen (mp:208-224) and the encoding half
// of its to_line (mp:186-197), on the engine's copy of the record: options is
// `defaults` when nil or empty (an absent options is neither, the symbol
// :absent is not empty, so a parsed record without options is written
// without them); a nil or empty fstype is refused (an absent one is not, so a
// parsed record without fstype is re-emitted, spec 5.4); then every space of
// device and name is written `\040`. The engine's join does the rest: only
// device and name are mandatory.
func mountPreGen(r *parsedfile.Record) error {
	if o := r.Fields["options"]; o.Kind == parsedfile.Nil || o.Kind == parsedfile.Str && o.S == "" {
		r.Fields["options"] = parsedfile.Value{Kind: parsedfile.Str, S: "defaults"}
	}
	if f := r.Fields["fstype"]; f.Kind == parsedfile.Nil || f.Kind == parsedfile.Str && f.S == "" {
		return &puppetError{fmt.Sprintf("Mount[%s]: Field 'fstype' is required", interpolated(r.Fields["name"]))}
	}
	for _, f := range []string{"device", "name"} {
		if v := r.Fields[f]; v.Kind == parsedfile.Str {
			v.S = strings.ReplaceAll(v.S, " ", octalSpace)
			r.Fields[f] = v
		}
	}
	return nil
}

// puppetError is an error mp: raises as a Puppet::Error (mp:217), which
// property.rb:490 passes through the ensure setter unwrapped (ensureError).
type puppetError struct{ msg string }

func (e *puppetError) Error() string { return e.msg }

// interpolated is how Ruby's string interpolation renders a field: a String as
// is, nil as "" and :absent as "absent".
func interpolated(v parsedfile.Value) string {
	switch v.Kind {
	case parsedfile.Str:
		return v.S
	case parsedfile.Absent:
		return "absent"
	}
	return ""
}

// mountPrefetchHook is mp:225-233: every entry in fstab is unmounted until the
// provider's prefetch proves different, and the name loses its trailing slashes
// as the name munge does.
func mountPrefetchHook(_ context.Context, _ string, rs []*parsedfile.Record) ([]*parsedfile.Record, error) {
	for _, r := range rs {
		if name := r.Fields["name"]; name.Kind == parsedfile.Str {
			name.S = trailingSlashes.ReplaceAllString(name.S, "${1}")
			r.Fields["name"] = name
		}
		if r.Type == "parsed" {
			r.Ensure = "unmounted"
		}
	}
	return rs, nil
}
