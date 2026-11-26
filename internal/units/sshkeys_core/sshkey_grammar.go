package sshkeyscore

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/rubyregexp"
)

// sp:<n> is lib/puppet/provider/sshkey/parsed.rb of puppetlabs-sshkeys_core.

// The known_hosts grammar's text patterns (sp:10-11). A grammar pattern sees
// one line, which holds no "\n", so RE2's text anchors equal Ruby's line
// anchors; \s is rubyregexp.Space (spec 4.5).
var (
	commentText = regexp.MustCompile(`^#`)                          // sp:10
	blankText   = regexp.MustCompile(`^` + rubyregexp.Space + `*$`) // sp:11
)

// absentNameError is sp:15's NoMethodError on a line with leading whitespace
// (the split's leading empty field is the absent text, so name is :absent), in
// the text of the oracle's ruby 3.2.11 (spec 5.2, M7 case 07e-leadws). It is not
// parsedfile.ErrNoRecord: it aborts the whole parse, as the exception aborts
// Ruby's prefetch.
var absentNameError = errors.New("undefined method `split' for :absent:Symbol")

// sshkeyClass is the sshkey provider `parsed` as a parsedfile.Class (sp:3-57).
// now is the clock of the header.
func sshkeyClass(newFileType func(target string) (parsedfile.FileType, error), now func() time.Time) *parsedfile.Class {
	grammar, err := parsedfile.NewGrammar(
		parsedfile.LineType{Name: "comment", Text: true, Match: parsedfile.RE2(commentText)},
		parsedfile.LineType{Name: "blank", Text: true, Match: parsedfile.RE2(blankText)},
		parsedfile.LineType{
			Name: "parsed", Fields: []string{"name", "type", "key"},
			PostParse: splitHostAliases, PreGen: joinHostAliases,
		},
	)
	if err != nil {
		panic("sshkey grammar: " + err.Error()) // the definition is static
	}
	return &parsedfile.Class{
		Name:              "parsed",
		TypeName:          "sshkey",
		Grammar:           grammar,
		DefaultTarget:     "/etc/ssh/ssh_known_hosts", // sp:35-52 on every platform this unit runs natively
		FileType:          newFileType,
		Now:               now,
		ResourceForRecord: sshkeyResourceForRecord,
	}
}

// splitHostAliases is the parsed line's post_parse (sp:14-18):
// name.split(',', -1), the first element is the name and the rest, empty
// elements kept, are host_aliases.
func splitHostAliases(r *parsedfile.Record) error {
	name := r.Fields["name"]
	if name.Kind != parsedfile.Str {
		return absentNameError
	}
	names := strings.Split(name.S, ",")
	r.Fields["name"] = parsedfile.Value{Kind: parsedfile.Str, S: names[0]}
	r.Fields["host_aliases"] = parsedfile.Value{Kind: parsedfile.List, L: append([]string{}, names[1:]...)}
	return nil
}

// joinHostAliases is the parsed line's pre_gen (sp:19-24): when host_aliases is
// set (an empty list included) name becomes [name, aliases].flatten.join(',')
// and host_aliases goes. Ruby's join renders nil as "" and :absent as "absent"
// (so an Absent host_aliases adds an alias "absent"); a missing field and Nil
// leave the record alone.
func joinHostAliases(r *parsedfile.Record) error {
	aliases, ok := r.Fields["host_aliases"]
	if !ok || aliases.Kind == parsedfile.Nil {
		return nil
	}
	parts := []string{rubyJoinText(r.Fields["name"])}
	switch aliases.Kind {
	case parsedfile.List:
		parts = append(parts, aliases.L...)
	default:
		parts = append(parts, rubyJoinText(aliases))
	}
	r.Fields["name"] = parsedfile.Value{Kind: parsedfile.Str, S: strings.Join(parts, ",")}
	delete(r.Fields, "host_aliases")
	return nil
}

// rubyJoinText is how Array#join renders a scalar element: a String as is,
// nil (here Nil or a missing field) as "" and :absent as "absent".
func rubyJoinText(v parsedfile.Value) string {
	switch v.Kind {
	case parsedfile.Str:
		return v.S
	case parsedfile.Absent:
		return "absent"
	}
	return ""
}

// sshkeyResourceForRecord is sp:54-57: the resource named "<name>@<type>".
// Ruby interpolates both, so a nil or absent part reads "" or "absent".
func sshkeyResourceForRecord(rec *parsedfile.Record, byName map[string]parsedfile.Resource) (parsedfile.Resource, bool) {
	res, ok := byName[rubyJoinText(rec.Fields["name"])+"@"+rubyJoinText(rec.Fields["type"])]
	return res, ok
}
