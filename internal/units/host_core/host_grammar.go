package hostcore

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/voxagent/voxagent/internal/lib/parsedfile"
)

// hp:<n> is lib/puppet/provider/host/parsed.rb of puppetlabs-host_core.

// The /etc/hosts grammar's patterns (hp:22-26). A grammar pattern sees one
// line, which holds no "\n", so RE2's text anchors equal Ruby's line anchors;
// \s is parsedfile.Space and \S parsedfile.NotSpace (spec 4.5). hosts_pattern's
// name class [^#\s+] is spelled out: Space cannot nest inside a class.
var (
	commentText  = regexp.MustCompile(`^#`)                                                                                                                                                                                                          // hp:21
	blankText    = regexp.MustCompile(`^` + parsedfile.Space + `*$`)                                                                                                                                                                                 // hp:22
	hostsPattern = regexp.MustCompile(`^([0-9a-f:]` + parsedfile.NotSpace + `+)` + parsedfile.Space + `+([^#+\t\n\v\f\r ]` + parsedfile.NotSpace + `+)` + parsedfile.Space + `*(.*?)?(?:` + parsedfile.Space + `*#` + parsedfile.Space + `*(.*))?$`) // hp:23
	spaceRun     = regexp.MustCompile(parsedfile.Space + `+`)
)

// incompleteMatcher is the `incomplete` type's pattern (hp:48), (?! (<hosts_pattern>)):
// a negative lookahead for a space followed by a pattern that starts with ^,
// which cannot match after a space, so the lookahead always fails and the type
// matches every line (spec 5.1). RE2 has no lookahead; the shared vectors pin
// this against Onigmo.
type incompleteMatcher struct{}

// Match implements parsedfile.Matcher: every line matches, with no captures.
func (incompleteMatcher) Match(string) ([]parsedfile.Capture, bool) { return nil, true }

// hostClass is the host provider `parsed` as a parsedfile.Class (hp:1-48).
// now is the clock of the header.
func hostClass(newFileType func(target string) (parsedfile.FileType, error), now func() time.Time) *parsedfile.Class {
	grammar, err := parsedfile.NewGrammar(
		parsedfile.LineType{Name: "comment", Text: true, Match: parsedfile.RE2(commentText)},
		parsedfile.LineType{Name: "blank", Text: true, Match: parsedfile.RE2(blankText)},
		parsedfile.LineType{
			Name: "parsed", Fields: []string{"ip", "name", "host_aliases", "comment"},
			Optional: []string{"host_aliases", "comment"}, Match: parsedfile.RE2(hostsPattern),
			PostParse: normalizeHost, ToLine: hostToLine,
		},
		parsedfile.LineType{Name: "incomplete", Text: true, Match: incompleteMatcher{}},
	)
	if err != nil {
		panic("host grammar: " + err.Error()) // the definition is static
	}
	return &parsedfile.Class{
		Name:          "parsed",
		TypeName:      "host",
		Grammar:       grammar,
		DefaultTarget: "/etc/hosts", // hp:2-9 on every platform this unit runs natively
		FileType:      newFileType,
		Now:           now,
	}
}

// normalizeHost is the parsed line's post_parse (hp:27-33): an absent comment
// is "" (so it matches an empty-string comment) and the aliases' whitespace
// runs become single spaces.
func normalizeHost(r *parsedfile.Record) error {
	if c := r.Fields["comment"]; c.Kind == parsedfile.Nil || c.Kind == parsedfile.Absent {
		r.Fields["comment"] = parsedfile.Value{Kind: parsedfile.Str, S: ""}
	}
	if a := r.Fields["host_aliases"]; a.Kind == parsedfile.Str {
		r.Fields["host_aliases"] = parsedfile.Value{Kind: parsedfile.Str, S: spaceRun.ReplaceAllLiteralString(a.S, " ")}
	}
	return nil
}

// hostToLine is the parsed line's to_line (hp:34-46): ip, name, the aliases
// unless nil or absent (an empty string is written, spec 8 item 2), then the
// comment unless it is empty. The provider's property hash holds the aliases
// as one string (OrderedList joins its should with a space); a list is joined
// the same way. An absent comment is not empty to Ruby, which writes the text
// "absent"; Nil is the same as a missing field here.
func hostToLine(r parsedfile.Record) (string, error) {
	for _, name := range []string{"ip", "name"} {
		if v := r.Fields[name]; v.Kind == parsedfile.Nil || v.Kind == parsedfile.Absent {
			return "", errors.New(name + " is a required attribute for hosts")
		}
	}
	var b strings.Builder
	b.WriteString(r.Fields["ip"].S + "\t" + r.Fields["name"].S)
	switch a := r.Fields["host_aliases"]; a.Kind {
	case parsedfile.Str:
		b.WriteString("\t" + a.S)
	case parsedfile.List:
		b.WriteString("\t" + strings.Join(a.L, " "))
	}
	switch c := r.Fields["comment"]; c.Kind {
	case parsedfile.Str:
		if c.S != "" {
			b.WriteString("\t# " + c.S)
		}
	case parsedfile.Absent:
		b.WriteString("\t# absent")
	}
	return b.String(), nil
}
