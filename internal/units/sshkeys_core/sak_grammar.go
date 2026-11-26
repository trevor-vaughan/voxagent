package sshkeyscore

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/rubyregexp"
	"github.com/voxagent/voxagent/internal/runlog"
)

// The authorized_keys grammar's text patterns (kp:11-12, kp:17, kp:34-35 and
// kt:169-174). A grammar pattern sees one line, which holds no "\n", so RE2's
// text anchors equal Ruby's line anchors (the RTS is (?m): it is applied to the
// generated line, which can hold line breaks, and Ruby's sub finds the first
// line start followed by whitespace, fp:353-361); \s is rubyregexp.Space (spec 4.5).
var (
	sakCommentText = regexp.MustCompile(`^` + rubyregexp.Space + `*#`)             // kp:11
	sakBlankText   = regexp.MustCompile(`^` + rubyregexp.Space + `*$`)             // kp:12
	sakRTS         = regexp.MustCompile(`(?m)^` + rubyregexp.Space + `+`)          // kp:17, 34
	sakKeyV1       = regexp.MustCompile(`^(?:(.+) )?(\d+) (\d+) (\d+)(?: (.+))?$`) // kp:35

	// kt:169-174's REGEX: the x flag drops the layout whitespace of the
	// alternation, the space in [^ ] is kept, and the dots of the .com types
	// stay unescaped as Ruby has them.
	sakKeyLine = regexp.MustCompile(`^(?:(.+)` + rubyregexp.Space + `+)?(` + strings.Join([]string{
		"ssh-dss", "ssh-ed25519", "ssh-rsa", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521",
		"ecdsa-sk", "ed25519-sk", "sk-ecdsa-sha2-nistp256@openssh.com", "sk-ssh-ed25519@openssh.com",
		"ssh-rsa-cert-v01@openssh.com", "ssh-ed25519-cert-v01@openssh.com", "ssh-dss-cert-v01@openssh.com",
		"ecdsa-sha2-nistp256-cert-v01@openssh.com", "ecdsa-sha2-nistp384-cert-v01@openssh.com",
		"ecdsa-sha2-nistp521-cert-v01@openssh.com",
	}, "|") + `)` + rubyregexp.Space + `+([^ ]+)` + rubyregexp.Space + `*(.*)$`)

	// parse_options' scanner patterns (kp:111-121), each anchored where the
	// StringScanner is.
	optionSkip      = regexp.MustCompile(`^[ \t]*`)
	optionLong      = regexp.MustCompile(`^[-a-z0-9A-Z_]+=".*?[^\\]"`)
	optionBare      = regexp.MustCompile(`^[-a-z0-9A-Z_]+`)
	optionSeparator = regexp.MustCompile(`^[ \t]*,[ \t]*`)
)

// sakClass is the ssh_authorized_key provider `parsed` as a parsedfile.Class
// (kp:3-35, 126-137). now is the clock of the header. DefaultTarget is "", which
// reads as empty (kp:7, pf:383).
func sakClass(newFileType func(target string) (parsedfile.FileType, error), now func() time.Time) *parsedfile.Class {
	grammar, err := parsedfile.NewGrammar(
		parsedfile.LineType{Name: "comment", Text: true, Match: parsedfile.RE2(sakCommentText)},
		parsedfile.LineType{Name: "blank", Text: true, Match: parsedfile.RE2(sakBlankText)},
		parsedfile.LineType{
			Name: "parsed", Fields: []string{"options", "type", "key", "name"}, Optional: []string{"options"},
			RTS: sakRTS, Match: parsedfile.RE2(sakKeyLine),
			PostParse: sakPostParse, PreGen: sakPreGen,
		},
		parsedfile.LineType{
			Name: "key_v1", Fields: []string{"options", "bits", "exponent", "modulus", "name"}, Optional: []string{"options"},
			RTS: sakRTS, Match: parsedfile.RE2(sakKeyV1),
		},
	)
	if err != nil {
		panic("ssh_authorized_key grammar: " + err.Error()) // the definition is static
	}
	return &parsedfile.Class{
		Name:          "parsed",
		TypeName:      "ssh_authorized_key",
		Grammar:       grammar,
		DefaultTarget: "",
		FileType:      newFileType,
		Now:           now,
		PrefetchHook:  sakPrefetchHook,
	}
}

// sakPostParse is the parsed line's post_parse (kp:19-23): an empty name is
// "", no options are [Absent] (a lone Absent is that list, as the vectors
// decode it) and a String of options is parse_options' list.
func sakPostParse(r *parsedfile.Record) error {
	if r.Fields["name"].Kind == parsedfile.Absent {
		r.Fields["name"] = parsedfile.Value{Kind: parsedfile.Str}
	}
	switch options := r.Fields["options"]; options.Kind {
	case parsedfile.Nil:
		r.Fields["options"] = parsedfile.Value{Kind: parsedfile.Absent}
	case parsedfile.Str:
		r.Fields["options"] = parsedfile.Value{Kind: parsedfile.List, L: parseOptions(options.S)}
	}
	return nil
}

// parseOptions is parse_options (kp:105-124): the comma-separated list of
// key="value" and bare-word elements, up to the first thing that is neither.
func parseOptions(options string) []string {
	result := []string{}
	for rest := options; rest != ""; {
		rest = rest[len(optionSkip.FindString(rest)):]
		out := optionLong.FindString(rest)
		if out == "" {
			out = optionBare.FindString(rest)
		}
		if out == "" {
			break
		}
		result = append(result, out)
		rest = rest[len(out):]
		rest = rest[len(optionSeparator.FindString(rest)):]
	}
	return result
}

// sakPreGen is the parsed line's pre_gen (kp:24-29): a record the prefetch hook
// named loses that name, no options (Absent) is the empty list, and the list is
// joined with ",". Ruby's include? raises on a record without options; no record
// reaches here without them (the grammar and the type's default give [Absent]),
// so that state is left alone.
func sakPreGen(r *parsedfile.Record) error {
	if r.Flags["unnamed"] {
		r.Fields["name"] = parsedfile.Value{Kind: parsedfile.Str}
	}
	switch options := r.Fields["options"]; options.Kind {
	case parsedfile.Absent:
		r.Fields["options"] = parsedfile.Value{Kind: parsedfile.Str}
	case parsedfile.List:
		r.Fields["options"] = parsedfile.Value{Kind: parsedfile.Str, S: strings.Join(options.L, ",")}
	}
	return nil
}

// sakPrefetchHook is kp:126-137, run per target: each parsed record without a
// name is flagged unnamed and named "<target>:unnamed-<n>", n from 1 in file
// order, so a purge can tell it from the keys the catalog manages (PUP-3357).
func sakPrefetchHook(ctx context.Context, _ string, rs []*parsedfile.Record) ([]*parsedfile.Record, error) {
	index := 0
	for _, rec := range rs {
		if name := rec.Fields["name"]; rec.Type != "parsed" || name.Kind != parsedfile.Str || name.S != "" {
			continue
		}
		index++
		rec.Flags["unnamed"] = true
		name := rec.Target + ":unnamed-" + strconv.Itoa(index)
		rec.Fields["name"] = parsedfile.Value{Kind: parsedfile.Str, S: name}
		runlog.Debugf(ctx, "Puppet", "generating name for on-disk ssh_authorized_key %s: %s", rec.Fields["key"].S, name)
	}
	return rs, nil
}
