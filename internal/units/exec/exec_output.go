package exec

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/command"
)

// logoutputMode reads Exec's logoutput (type/exec.rb:271, newvalues(:true,
// :false, :on_failure), default :on_failure) as "true", "false" or
// "on_failure". An invalid value is Ruby's error; its value is shown as
// Ruby's inspect does, a string quoted, and replaced quotes included when the
// catalog marks logoutput (D-78: Ruby leaks it).
func logoutputMode(r catalog.Resource) (string, error) {
	raw, ok := r.Parameters["logoutput"]
	if !ok {
		return "on_failure", nil
	}
	switch v := raw.(type) {
	case bool:
		return fmt.Sprint(v), nil
	case string:
		if v == "true" || v == "false" || v == "on_failure" {
			return v, nil
		}
	}
	shown := fmt.Sprintf("%v", raw)
	if _, isString := raw.(string); isString {
		shown = `"` + shown + `"`
	}
	if slices.Contains(r.SensitiveParameters, "logoutput") {
		shown = "[redacted]"
	}
	return "", fmt.Errorf("Parameter logoutput failed on %s: Invalid value %s. Valid values are true, false, on_failure.",
		r.Ref(), shown)
}

// outputNotices is type/exec.rb sync's logoutput branch (:120-190): the lines
// a run's output is logged as at the returns property, or nil when the mode
// does not log it. A marked command's output is never read: one
// "[output redacted]" line stands for it, empty output included.
// Otherwise output invalid in the locale's encoding is Ruby's split error
// (the caller makes it the change's error).
func outputNotices(mode string, failed bool, output string, redact bool) ([]string, error) {
	if mode == "false" || (mode == "on_failure" && !failed) {
		return nil, nil
	}
	if redact {
		return []string{"[output redacted]"}, nil
	}
	enc := outputEncoding(os.Getenv)
	valid := utf8.ValidString(output)
	if enc == "US-ASCII" {
		valid = !strings.ContainsFunc(output, func(c rune) bool { return c >= utf8.RuneSelf })
	}
	if !valid {
		return nil, fmt.Errorf("invalid byte sequence in %s", enc)
	}
	return command.SplitOutput(output), nil
}

// outputEncoding is Ruby's Encoding.default_external for the locale: the
// codeset of the first set, non-empty one of LC_ALL, LC_CTYPE and LANG. A
// UTF-8 codeset (UTF-8 or utf8, any case) is "UTF-8"; anything else, no
// codeset, C and POSIX included, is "US-ASCII". Ruby would use any other
// codeset as its own encoding; voxagent treats it as US-ASCII (D-79).
func outputEncoding(getenv func(string) string) string {
	for _, name := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		locale := getenv(name)
		if locale == "" {
			continue
		}
		_, codeset, _ := strings.Cut(locale, ".")
		codeset, _, _ = strings.Cut(codeset, "@")
		if strings.EqualFold(codeset, "UTF-8") || strings.EqualFold(codeset, "utf8") {
			return "UTF-8"
		}
		return "US-ASCII"
	}
	return "US-ASCII"
}
