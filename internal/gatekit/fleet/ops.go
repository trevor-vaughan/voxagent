package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/voxagent/voxagent/internal/gatekit/pycompat"
)

// requiredFacts are the facts a capture must hold for the normalised
// comparison to mean anything: the node's identity, its OS, and the fact the
// fleet's own hiera layer keys on.
var requiredFacts = []string{"networking", "os", "fleet_tier"}

// minimumFacts is the size below which a capture is taken as truncated.
const minimumFacts = 20

// CheckFacts returns why the fact capture `puppet facts find` wrote to path
// cannot be trusted, or "" when it can.
//
// `puppet facts find` exits 0 even when individual resolvers fail -- on this
// host the lscpu resolver errors on every run and the affected facts are
// simply absent. A truncated capture would normalise BOTH sides onto the same
// wrong facts, the comparison would still report IDENTICAL, and the delta the
// implementations check exists to produce would be attributed to the
// compiler. So the capture is validated rather than its exit status trusted.
//
// An error means the file is not a fact capture at all.
func CheckFacts(path, node string) (string, error) {
	doc, err := pycompat.DecodeFile(path)
	if err != nil {
		return "", err
	}
	facts, ok := doc.(map[string]any)
	if !ok {
		return "", fmt.Errorf("%s holds %s, not a fact capture", path, pycompat.Shape(doc))
	}
	raw := facts["values"]
	values := map[string]any{}
	if pycompat.Truthy(raw) {
		if values, ok = raw.(map[string]any); !ok {
			return "", fmt.Errorf("%s's values are %s, not an object", path, pycompat.Shape(raw))
		}
	}
	var missing []string
	for _, k := range requiredFacts {
		if _, present := values[k]; !present {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Sprintf("capture for %s is missing required facts: %s", node, strings.Join(missing, ", ")), nil
	}
	if len(values) < minimumFacts {
		return fmt.Sprintf("capture for %s has only %d facts; expected a full set", node, len(values)), nil
	}
	return "", nil
}

// ExportQueryTimeout bounds one OpenVoxDB query, as the Python poll did.
const ExportQueryTimeout = 5 * time.Second

// ErrNotExported reports that OpenVoxDB answered and holds no exported
// resource with the title asked about.
var ErrNotExported = errors.New("no exported resource with that title")

// Exported asks the OpenVoxDB at base whether a resource titled title has been
// exported. nil means it has; ErrNotExported means OpenVoxDB answered that it
// has not; any other error means the query itself failed. The caller polls on
// any non-nil result, so the distinction is for the reader of its message.
func Exported(base, title string) error {
	query, err := json.Marshal([]any{"and", []any{"=", "exported", true}, []any{"=", "title", title}})
	if err != nil {
		return err
	}
	client := http.Client{Timeout: ExportQueryTimeout}
	resp, err := client.Get(base + "/pdb/query/v4/resources?" + url.Values{"query": {string(query)}}.Encode())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("OpenVoxDB answered %s", resp.Status)
	}
	var answer any
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&answer); err != nil {
		return fmt.Errorf("OpenVoxDB's answer is not JSON: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("OpenVoxDB's answer is not JSON: extra data after the first value")
	}
	if !pycompat.Truthy(answer) {
		return ErrNotExported
	}
	return nil
}

// MutateManifest replaces the single occurrence of original in the file at
// path with mutated. It refuses, changing nothing, when original does not
// occur exactly once: the negative control must know precisely what it broke.
// The file is rewritten byte for byte apart from the replacement.
func MutateManifest(path, original, mutated string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("could not mutate %s: %w", path, err)
	}
	if !utf8.Valid(data) {
		return fmt.Errorf("could not mutate %s: not valid UTF-8", path)
	}
	text := string(data)
	if n := strings.Count(text, original); n != 1 {
		return fmt.Errorf("expected exactly 1 occurrence of the target line in %s, found %d", path, n)
	}
	// Rewritten in place -- truncated and written, the same inode -- because
	// the manifest is bind-mounted into the node containers, and a file
	// replaced by rename would leave them reading the old one. The file
	// exists, so the mode argument is never used.
	if err := os.WriteFile(path, []byte(strings.Replace(text, original, mutated, 1)), 0o644); err != nil {
		return fmt.Errorf("could not mutate %s: %w", path, err)
	}
	return nil
}
