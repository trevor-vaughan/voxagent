package ral

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/augeas"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

// The generic augeas type (augeas_core 2.1.0): its changes parser, a port of
// provider/augeas/augeas.rb parse_commands (P:73-157), and the shape check
// that decides whether a resource is native (spec §0 row 6, §3).

// augeasCommand is one parsed command. Name is as typed (remove stays remove);
// Args are in parse order, so ins and insert carry label, where, path.
type augeasCommand struct {
	Name string
	Args []string
}

// augeasParams is an Augeas resource's settled values. Context is the
// context without parse_commands' trailing slash: the provider's load
// restriction needs the raw one. HasIncl is whether incl is given: the
// provider tests it for truth (P:164, :182), so an empty incl counts.
type augeasParams struct {
	Title, Context, Incl, Lens string
	HasIncl                    bool
	Commands                   []augeasCommand
}

// augeasFormals is COMMANDS (P:40-63): the argument types each command
// takes, in order.
var augeasFormals = map[string][]byte{
	"set": {'p', 's'}, "setm": {'p', 's', 's'},
	"rm": {'p'}, "remove": {'p'}, "clear": {'p'}, "clearm": {'p', 's'}, "touch": {'p'},
	"mv": {'p', 'p'}, "move": {'p', 'p'}, "rename": {'p', 's'},
	"insert": {'s', 's', 'p'}, "ins": {'s', 's', 'p'},
	"get": {'p', 'c', 's'}, "values": {'p', 'g'},
	"defvar": {'s', 'p'}, "defnode": {'s', 'p', 's'}, "match": {'p', 'g'},
	"size": {'c', 'i'}, "include": {'s'}, "not_include": {'s'},
	"==": {'g'}, "!=": {'g'},
}

// augeasNative is the commands native runs (spec §0 row 6); the rest of
// COMMANDS parse but go to the host.
var augeasNative = []string{"set", "rm", "remove", "clear", "touch", "ins", "insert", "defvar", "defnode", "mv", "move"}

// rubySpace is Ruby's /\s/: ASCII space, \t, \n, \v, \f, \r.
func rubySpace(c byte) bool {
	return c == ' ' || (c >= '\t' && c <= '\r')
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// lineScanner is the part of StringScanner parse_commands uses. The input is
// ASCII (parseAugeasChanges refuses anything else), so a byte is a character.
type lineScanner struct {
	s   string
	pos int
}

func (sc *lineScanner) eos() bool { return sc.pos >= len(sc.s) }

// skipSpace is skip(/\s+/).
func (sc *lineScanner) skipSpace() {
	for !sc.eos() && rubySpace(sc.s[sc.pos]) {
		sc.pos++
	}
}

// getch is StringScanner#getch: the next character, or 0 and false at the end.
func (sc *lineScanner) getch() (byte, bool) {
	if sc.eos() {
		return 0, false
	}
	sc.pos++
	return sc.s[sc.pos-1], true
}

// skipPathRun is skip(/([^\]\[\s\\'"]|\\.)+/) (P:103): a backslash pairs with
// the next character unless that is a newline, which . does not match.
func (sc *lineScanner) skipPathRun() {
	for !sc.eos() {
		switch c := sc.s[sc.pos]; {
		case c == '\\':
			if sc.pos+1 >= len(sc.s) || sc.s[sc.pos+1] == '\n' {
				return
			}
			sc.pos += 2
		case c == '[' || c == ']' || c == '\'' || c == '"' || rubySpace(c):
			return
		default:
			sc.pos++
		}
	}
}

// scanPath is the :path formal (P:97-124), before the context is applied. The
// returned text has its outer ticks stripped and one trailing slash chomped.
func (sc *lineScanner) scanPath() (string, error) {
	start := sc.pos
	nbracket, inSingle, inDouble := 0, false, false
	for {
		sc.skipPathRun()
		ch, _ := sc.getch()
		switch ch {
		case '[':
			nbracket++
		case ']':
			nbracket--
		case '\'':
			inSingle = !inSingle
		case '"':
			inDouble = !inDouble
		}
		if nbracket < 0 {
			return "", errors.New("unmatched [")
		}
		if (nbracket == 0 && !inSingle && !inDouble && rubySpace(ch)) || sc.eos() {
			break
		}
	}
	end := sc.pos
	if !sc.eos() {
		end-- // the whitespace that ended the argument
	}
	p := sc.s[start:end]
	if p != "" && (p[0] == '\'' || p[0] == '"') {
		// p[1, p.size-2] strips a first and a last character without checking
		// that they match; for a lone tick it is nil and chomp! raises.
		if len(p) < 2 {
			return "", errors.New("undefined method `chomp!' for nil:NilClass")
		}
		p = p[1 : len(p)-1]
	}
	return strings.TrimSuffix(p, "/"), nil
}

// scanString is the :string formal (P:125-141). ok is false when a bare word
// finds nothing.
func (sc *lineScanner) scanString() (string, bool) {
	if !sc.eos() && (sc.s[sc.pos] == '\'' || sc.s[sc.pos] == '"') {
		delim := sc.s[sc.pos]
		sc.pos++
		start := sc.pos
		// scan(/([^\\delim]|(\\.))*/)
	scan:
		for !sc.eos() {
			switch c := sc.s[sc.pos]; {
			case c == '\\':
				if sc.pos+1 >= len(sc.s) || sc.s[sc.pos+1] == '\n' {
					break scan
				}
				sc.pos += 2
			case c == delim:
				break scan
			default:
				sc.pos++
			}
		}
		s := sc.s[start:sc.pos]
		sc.getch() // the closing quote, taken blind
		return strings.ReplaceAll(s, `\`+string(delim), string(delim)), true
	}
	start := sc.pos
	for !sc.eos() && !rubySpace(sc.s[sc.pos]) {
		sc.pos++
	}
	return sc.s[start:sc.pos], sc.pos > start
}

// scanComparator is scan(/(==|!=|=~|<=|>=|<|>)/) (P:143). Ruby prints the
// rest of the line when it fails; native does not.
func (sc *lineScanner) scanComparator() (string, bool) {
	rest := sc.s[sc.pos:]
	for _, c := range []string{"==", "!=", "=~", "<=", ">=", "<", ">"} {
		if strings.HasPrefix(rest, c) {
			sc.pos += len(c)
			return c, true
		}
	}
	return "", false
}

// scanInt is scan(/\d+/).to_i (P:149): no digits is 0. Digits only, no
// leading zeros.
func (sc *lineScanner) scanInt() string {
	start := sc.pos
	for !sc.eos() && sc.s[sc.pos] >= '0' && sc.s[sc.pos] <= '9' {
		sc.pos++
	}
	if d := strings.TrimLeft(sc.s[start:sc.pos], "0"); d != "" {
		return d
	}
	return "0"
}

// parseAugeasLine parses one stripped, non-empty line. context already has
// its trailing slash.
func parseAugeasLine(line, context string) (augeasCommand, error) {
	sc := &lineScanner{s: line}
	// scan(/\w+|==|!=/)
	for !sc.eos() && isWordByte(sc.s[sc.pos]) {
		sc.pos++
	}
	if sc.pos == 0 {
		if strings.HasPrefix(line, "==") || strings.HasPrefix(line, "!=") {
			sc.pos = 2
		}
	}
	name := line[:sc.pos]
	formals, known := augeasFormals[name]
	if !known {
		return augeasCommand{}, fmt.Errorf("Unknown command %s", name)
	}
	cmd := augeasCommand{Name: name}
	for i, f := range formals {
		sc.skipSpace()
		narg := i + 1
		switch f {
		case 'p':
			p, err := sc.scanPath()
			if err != nil {
				return augeasCommand{}, err
			}
			if !strings.HasPrefix(p, "$") && !strings.HasPrefix(p, "/") {
				p = context + p
			}
			cmd.Args = append(cmd.Args, p)
		case 's':
			s, ok := sc.scanString()
			if !ok {
				return augeasCommand{}, fmt.Errorf("missing string argument %d for %s", narg, name)
			}
			cmd.Args = append(cmd.Args, s)
		case 'c':
			c, ok := sc.scanComparator()
			if !ok {
				return augeasCommand{}, fmt.Errorf("invalid comparator for command %s", name)
			}
			cmd.Args = append(cmd.Args, c)
		case 'i':
			cmd.Args = append(cmd.Args, sc.scanInt())
		case 'g':
			cmd.Args = append(cmd.Args, sc.s[sc.pos:])
			sc.pos = len(sc.s)
		}
	}
	return cmd, nil
}

// flattenChanges is parse_commands' split and flatten (P:80-81): a top-level
// string is split on "\n" with trailing empty fields dropped, an array is
// flattened to any depth without splitting its members, and nil members are
// skipped (P:84). Any other value is a NoMethodError in Ruby, an error here.
func flattenChanges(changes any) ([]string, error) {
	if s, isString := changes.(string); isString {
		lines := strings.Split(s, "\n")
		for len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		return lines, nil
	}
	array, isArray := changes.([]any)
	if !isArray {
		return nil, fmt.Errorf("changes %s is not a string or an array", rubyvalue.InspectValue(changes))
	}
	var out []string
	for _, e := range array {
		switch m := e.(type) {
		case nil:
		case string:
			out = append(out, m)
		case []any:
			sub, err := flattenChanges(m)
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
		default:
			return nil, fmt.Errorf("changes member %s is not a string", rubyvalue.InspectValue(e))
		}
	}
	return out, nil
}

// parseAugeasChanges is parse_commands (P:73-157). context is the settled
// context; the trailing slash is added here as P:76-78 does. A line with a
// non-ASCII byte is an error: Ruby indexes characters with the scanner's byte
// positions, so its result for such a line is not reproduced. The error text
// is Ruby's; nothing is printed (Ruby's stray puts on a bad comparator
// included).
func parseAugeasChanges(changes any, context string) ([]augeasCommand, error) {
	if context != "" && !strings.HasSuffix(context, "/") {
		context += "/"
	}
	lines, err := flattenChanges(changes)
	if err != nil {
		return nil, err
	}
	var cmds []augeasCommand
	for _, line := range lines {
		line = rubyvalue.Strip(line)
		if line == "" {
			continue
		}
		if slices.ContainsFunc([]byte(line), func(b byte) bool { return b >= utf8.RuneSelf }) {
			return nil, errors.New("non-ASCII byte in changes")
		}
		cmd, err := parseAugeasLine(line, context)
		if err != nil {
			return nil, err
		}
		cmds = append(cmds, cmd)
	}
	return cmds, nil
}

// printableASCII reports whether every byte of s is 0x20-0x7E.
func printableASCII(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r > 0x7E })
}

// augeasASCII reads a parameter that must be a string of printable ASCII;
// absent is "" with present false. Anything else is the refusal.
func augeasASCII(r catalog.Resource, name string) (s string, present bool, err error) {
	v := r.Parameters[name]
	if v == nil {
		return "", false, nil
	}
	s, isString := v.(string)
	if !isString || !printableASCII(s) {
		return "", false, augeas.Unimplemented(r, name)
	}
	return s, true, nil
}

// parseAugeas settles an Augeas resource (spec §3) or refuses it (§0 row 6)
// with an *rubyvalue.UnimplementedValue naming the attribute. force, onlyif, root,
// load_path, type_check, returns and show_diff keep no native value: only the
// shapes native reproduces pass.
func parseAugeas(r catalog.Resource) (augeasParams, error) {
	p := augeasParams{Title: r.Title}
	if err := augeas.Prelude(r); err != nil {
		return p, err
	}
	var hasLens bool
	var err error
	if p.Lens, hasLens, err = augeasASCII(r, "lens"); err != nil {
		return p, err
	}
	if p.Incl, p.HasIncl, err = augeasASCII(r, "incl"); err != nil {
		return p, err
	}
	if hasLens != p.HasIncl {
		// T:162-166: lens XOR incl; name the one that is missing.
		if hasLens {
			return p, augeas.Unimplemented(r, "incl")
		}
		return p, augeas.Unimplemented(r, "lens")
	}
	// T:64-70: an explicit context stays as given, even ''.
	var hasContext bool
	if p.Context, hasContext, err = augeasASCII(r, "context"); err != nil {
		return p, err
	}
	if !hasContext && p.HasIncl {
		p.Context = "/files" + p.Incl
	}
	// Only the values that need no native handling are accepted.
	for _, c := range []struct {
		name string
		ok   func(any) bool
	}{
		{"onlyif", func(v any) bool { return v == "" }},
		{"root", func(v any) bool { return v == "/" }},
		{"load_path", func(v any) bool { return v == "" }},
		{"force", func(v any) bool { return v == false }},
		{"type_check", func(v any) bool { return v == false || v == "false" }},
		{"returns", func(v any) bool { return v == float64(0) }},
		{"show_diff", func(v any) bool { _, err := rubyvalue.Bool(v); return err == nil }},
	} {
		if v := r.Parameters[c.name]; v != nil && !c.ok(v) {
			return p, augeas.Unimplemented(r, c.name)
		}
	}
	if r.Parameters["changes"] == nil {
		return p, augeas.Unimplemented(r, "changes")
	}
	if p.Commands, err = parseAugeasChanges(r.Parameters["changes"], p.Context); err != nil || len(p.Commands) == 0 {
		return p, augeas.Unimplemented(r, "changes")
	}
	for _, c := range p.Commands {
		if !slices.Contains(augeasNative, c.Name) || slices.ContainsFunc(c.Args, func(a string) bool { return !printableASCII(a) }) {
			return p, augeas.Unimplemented(r, "changes")
		}
	}
	return p, nil
}

// rubyInspectStrings is Array#inspect of printable-ASCII strings (spec §5):
// each element double-quoted, with " and \ backslash-escaped and # escaped
// when it starts an interpolation (#{, #$, #@).
func rubyInspectStrings(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		var b strings.Builder
		b.WriteByte('"')
		for j := 0; j < len(a); j++ {
			switch c := a[j]; {
			case c == '"' || c == '\\':
				b.WriteByte('\\')
				b.WriteByte(c)
			case c == '#' && j+1 < len(a) && (a[j+1] == '{' || a[j+1] == '$' || a[j+1] == '@'):
				b.WriteString(`\#`)
			default:
				b.WriteByte(c)
			}
		}
		b.WriteByte('"')
		parts[i] = b.String()
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
