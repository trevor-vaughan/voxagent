package parsedfile

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

// Kind says what a Value holds. Ruby tells nil, the symbol :absent and the
// empty string apart (match mode, split mode and join treat them
// differently), so the Go model does too.
type Kind uint8

const (
	Nil    Kind = iota // Ruby nil: no value, or a capture group that did not participate
	Absent             // Ruby :absent
	Str                // a String
	List               // an Array of Strings (post-parse lists: cron time fields, sshkey aliases)
)

// Value is one field of a Record. The zero Value is Nil.
//
// Ruby's :absent inside an Array (fp:110 tests == [:absent]) is a Value of
// Kind Absent; a List holds Strings only, so the list ["absent"] is generated
// as that string list.
type Value struct {
	Kind Kind
	S    string
	L    []string
}

// Record is a parsed line, or a record to generate a line from.
type Record struct {
	Type   string           // the LineType name (Ruby's record_type)
	Line   string           // text records: the line verbatim
	Fields map[string]Value // record fields and attributes hooks add
	Flags  map[string]bool  // consumer flags (cron: unmanaged, skip)
	Target string
	OnDisk bool
	Ensure string // "present", "absent", or a consumer value; only "absent" is dropped at write
}

// LineType defines one kind of line (Ruby's text_line and record_line, fp:301-323).
type LineType struct {
	Name   string
	Text   bool    // a text line (the whole line is kept); else a record line
	Match  Matcher // text: the selector, required; record: whole-line captures, nil = split mode
	Fields []string
	// Optional lists the fields that may be Absent when generating.
	Optional []string
	// Absent is the text of an absent field on the wire; the in-memory value is
	// always Absent. Record default "" (fp:84).
	Absent string
	// Separator splits a line in split mode; nil means Ruby's /\s+/ (fp:85).
	Separator *regexp.Regexp
	// Joiner joins fields when generating and when rolling up; "" means " "
	// (fp:86; the Ruby docstring's "\t" is wrong).
	Joiner string
	// RTS is removed from a generated line with Ruby's sub, the first match
	// only (fp:353-361); nil keeps the line. It is compiled without (?m), so
	// ^ and $ anchor the whole text, not each line as Ruby's do: a consumer
	// whose generated line can hold a line break must account for that.
	RTS *regexp.Regexp
	// PostParse runs on every record of this type after parsing (fp:276). It
	// returns false for a line it cannot make a record of, which fails the
	// parse with a ParseError for that line, as an error raised there does.
	PostParse func(*Record) bool
	// PreGen runs on a shallow copy before generating (fp:341-344).
	PreGen func(*Record)
	// ToLine replaces the join and RTS; its result is used verbatim (fp:349).
	ToLine func(Record) (string, error)
}

// reservedFields are the names a record type cannot use as fields (fp:39).
var reservedFields = []string{"record_type", "target", "on_disk"}

var defaultSeparator = regexp.MustCompile(Space + `+`)

type lineType struct {
	LineType
	optional map[string]bool
}

// Grammar is an ordered list of line types: the first that matches a line wins.
type Grammar struct {
	types  []lineType
	byName map[string]*lineType
}

// NewGrammar defines the line types in order. It refuses what Ruby's
// record_line, text_line and new_line_type raise ArgumentError for, with their
// texts: a record type without Fields (nil), a text type without Match, a
// reserved field name and a duplicate name (fp:39-48, 301-323, 388-398).
func NewGrammar(types ...LineType) (*Grammar, error) {
	g := &Grammar{types: make([]lineType, 0, len(types)), byName: map[string]*lineType{}}
	seen := map[string]bool{}
	for _, t := range types {
		switch {
		case t.Text && t.Match == nil:
			return nil, errors.New("You must provide a :match regex for text lines")
		case !t.Text && t.Fields == nil:
			return nil, errors.New("Must include a list of fields")
		}
		for _, f := range t.Fields {
			if slices.Contains(reservedFields, f) {
				return nil, fmt.Errorf("Cannot have fields named %s", f)
			}
		}
		if seen[t.Name] {
			return nil, fmt.Errorf("Line type %s is already defined", t.Name)
		}
		seen[t.Name] = true
		if !t.Text {
			if t.Separator == nil {
				t.Separator = defaultSeparator
			}
			if t.Joiner == "" {
				t.Joiner = " "
			}
		}
		lt := lineType{LineType: t, optional: map[string]bool{}}
		for _, f := range t.Optional {
			lt.optional[f] = true
		}
		g.types = append(g.types, lt)
	}
	// g.types no longer grows, so the pointers are stable.
	for i := range g.types {
		g.byName[g.types[i].Name] = &g.types[i]
	}
	return g, nil
}

// Lines is Ruby's text.split("\n") (fp:244-248): leading and inner empty lines
// are kept, trailing ones dropped, so "" and "\n" give none. A "\r" stays in
// its line.
func (g *Grammar) Lines(text string) []string {
	lines := strings.Split(text, "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// ParseError is a line no line type matched. Line is the physical line number
// plus one: Ruby's counter starts at 1 and is incremented before the first
// line (fp:251-262). The ParsedFile layer sets File.
type ParseError struct {
	Line int
	Text string
	File string
}

func (e *ParseError) Error() string {
	return "Could not parse line " + rubyvalue.Inspect(e.Text) + rubyvalue.ErrorLocation(e.File, e.Line)
}

// Parse turns text into records, one per line (fp:251-264). The first line no
// type matches fails the whole parse with a *ParseError.
func (g *Grammar) Parse(text string) ([]*Record, error) {
	lines := g.Lines(text)
	records := make([]*Record, 0, len(lines))
	for i, line := range lines {
		if len(g.types) == 0 {
			return nil, errors.New("No record types defined; cannot parse lines")
		}
		r := g.parseLine(line)
		if r == nil {
			return nil, &ParseError{Line: i + 2, Text: line}
		}
		records = append(records, r)
	}
	return records, nil
}

// parseLine tries the types in definition order; the first match wins and its
// PostParse runs on the result (fp:267-285); a PostParse that rejects the line
// ends the search with nil.
func (g *Grammar) parseLine(line string) *Record {
	for i := range g.types {
		lt := &g.types[i]
		var r *Record
		if lt.Text {
			r = lt.textLine(line)
		} else {
			r = lt.recordLine(line)
		}
		if r == nil {
			continue
		}
		if lt.PostParse != nil && !lt.PostParse(r) {
			return nil
		}
		return r
	}
	return nil
}

// textLine is fp:163-165: the selector matches anywhere in the line.
func (lt *lineType) textLine(line string) *Record {
	if _, ok := lt.Match.Match(line); !ok {
		return nil
	}
	return &Record{Type: lt.Name, Line: line, Fields: map[string]Value{}, Flags: map[string]bool{}}
}

// recordLine is fp:173-235 without the process hook (no consumer defines one).
func (lt *lineType) recordLine(line string) *Record {
	r := &Record{Type: lt.Name, Fields: make(map[string]Value, len(lt.Fields)), Flags: map[string]bool{}}
	if lt.Match != nil {
		caps, ok := lt.Match.Match(line)
		if !ok {
			return nil
		}
		for i, f := range lt.Fields {
			switch {
			case i >= len(caps) || !caps[i].OK:
				r.Fields[f] = Value{Kind: Nil}
			case caps[i].S == lt.Absent:
				r.Fields[f] = Value{Kind: Absent}
			default:
				r.Fields[f] = Value{Kind: Str, S: caps[i].S}
			}
		}
		return r
	}

	// Split mode. A leading empty element is kept and shifts every field. A
	// missing element or one equal to the absent text is Absent; "" is a
	// value unless the absent text is "".
	parts := Split(line, lt.Separator)
	for _, f := range lt.Fields {
		if len(parts) > 0 && parts[0] != lt.Absent {
			r.Fields[f] = Value{Kind: Str, S: parts[0]}
		} else {
			r.Fields[f] = Value{Kind: Absent}
		}
		if len(parts) > 0 {
			parts = parts[1:]
		}
	}
	// Rollup (always on, fp:72,88): leftover elements are joined onto the last
	// field. An Absent last field joins as the symbol's text, "absent".
	if len(parts) > 0 && len(lt.Fields) > 0 {
		last := lt.Fields[len(lt.Fields)-1]
		first := r.Fields[last].S
		if r.Fields[last].Kind == Absent {
			first = "absent"
		}
		r.Fields[last] = Value{Kind: Str, S: strings.Join(append([]string{first}, parts...), lt.Joiner)}
	}
	return r
}

// ToLine generates the line of one record (fp:335-364).
func (g *Grammar) ToLine(r Record) (string, error) {
	lt, ok := g.byName[r.Type]
	if !ok {
		return "", fmt.Errorf("Invalid record type :%s", r.Type)
	}
	if lt.PreGen != nil {
		r.Fields = cloneOrNew(r.Fields)
		r.Flags = cloneOrNew(r.Flags)
		lt.PreGen(&r)
	}
	if lt.Text {
		return r.Line, nil
	}
	if lt.ToLine != nil {
		return lt.ToLine(r)
	}
	line, err := lt.join(r)
	if err != nil {
		return "", err
	}
	if lt.RTS != nil {
		if loc := lt.RTS.FindStringIndex(line); loc != nil {
			line = line[:loc[0]] + line[loc[1]:]
		}
	}
	return line, nil
}

// join is FileRecord#join (fp:105-120). Only Nil and Absent are absent; "" is a value.
func (lt *lineType) join(r Record) (string, error) {
	parts := make([]string, len(lt.Fields))
	for i, f := range lt.Fields {
		v := r.Fields[f]
		switch {
		case v.Kind == Nil || v.Kind == Absent:
			if !lt.optional[f] {
				return "", fmt.Errorf("Field '%s' is required", f)
			}
			parts[i] = lt.Absent
		case v.Kind == List:
			// Array#to_s is its inspect form.
			quoted := make([]string, len(v.L))
			for j, e := range v.L {
				quoted[j] = rubyvalue.Inspect(e)
			}
			parts[i] = "[" + strings.Join(quoted, ", ") + "]"
		default:
			parts[i] = v.S
		}
	}
	return strings.Join(parts, lt.Joiner), nil
}

// ToFile generates a file: the lines joined with "\n" plus a trailing "\n", so
// no records give "\n" (fp:326-332, trailing_separator defaults to true).
func (g *Grammar) ToFile(rs []*Record) (string, error) {
	lines := make([]string, len(rs))
	for i, r := range rs {
		line, err := g.ToLine(*r)
		if err != nil {
			return "", err
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n") + "\n", nil
}

// cloneOrNew copies m, giving hooks a map they may write to even when the
// caller's record has none.
func cloneOrNew[V any](m map[string]V) map[string]V {
	if m == nil {
		return map[string]V{}
	}
	return maps.Clone(m)
}
