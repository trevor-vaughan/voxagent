// Package puppetapi is an HTTP client for the Puppet CA API (/puppet-ca/v1)
// and the v3 agent API (/puppet/v3) as an OpenVox server serves them. Request
// shapes are copied from a measured Puppet agent run.
//
// Every response body is read under a cap after gzip
// decoding, which the Transport does: it offers gzip itself and decodes the
// response transparently, so no request sets Accept-Encoding.
package puppetapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
)

// Response caps, after decompression.
const (
	maxPEM       = 1 << 20   // CA bundle, CRL bundle, certificate
	maxSmallJSON = 64 << 10  // node, single file metadata, report response
	maxErrorBody = 64 << 10  // body of a non-success response
	maxCatalog   = 256 << 20 // catalog
	maxMetadatas = 256 << 20 // file_metadatas array
	maxMessage   = 4 << 10   // server message text kept in an HTTPError
)

// maxFileContent caps a streamed file_content body as maxMetadatas caps the
// listing; a variable only so tests can lower it.
var maxFileContent int64 = 256 << 20

// Accept headers per endpoint, as Puppet sends them.
const (
	acceptText    = "text/plain"
	acceptJSON    = "application/json"
	acceptCatalog = "application/vnd.puppet.rich+json, application/json"
	acceptOctet   = "application/octet-stream"
)

// checksumTypes is Puppet's supported_checksum_types joined by '.', sent with
// the catalog request; the server picks the first it knows.
const checksumTypes = "sha256.sha384.sha512.sha224.md5"

// Client talks to one Puppet server. Build it with New.
type Client struct {
	base string // https://<server>:<port>
	hc   *http.Client
}

// HTTPError is a response with a status the endpoint does not accept. Message
// is the server's {"message": …} text when the body is Puppet's error JSON,
// otherwise the body text; either way truncated to 4 KiB with control
// characters other than \n escaped, so it is safe to print.
type HTTPError struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *HTTPError) Error() string {
	s := fmt.Sprintf("puppetapi: %s %s: HTTP %d %s", e.Method, e.Path, e.Status, http.StatusText(e.Status))
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// CatalogForm is what the catalog request carries besides the certname. Facts
// is the JSON facts wrapper {"name", "values", "timestamp", "expiration"}.
type CatalogForm struct {
	Environment, TransactionUUID string
	Facts                        []byte
}

// Metadata is one file_metadata object. A null
// relative_path or destination decodes as "".
type Metadata struct {
	Path         string `json:"path"`
	RelativePath string `json:"relative_path"`
	Type         string `json:"type"`
	Destination  string `json:"destination"`
	Checksum     struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"checksum"`
	Mode  int `json:"mode"`
	Owner int `json:"owner"`
	Group int `json:"group"`
}

// MetadatasParams are the file_metadatas query parameters that differ between
// callers: pluginsync sends recurse=true, max_files=-1 and the VCS ignores; a
// recursive File source sends max_files=0 and no ignores.
// MaxFiles and Ignore are sent only when Recurse is set, as Puppet does.
type MetadatasParams struct {
	Recurse                  bool
	MaxFiles                 int
	Links, SourcePermissions string
	Ignore                   []string
}

func baseURL(server string, port int) string {
	return "https://" + net.JoinHostPort(server, strconv.Itoa(port))
}

// CA fetches the CA bundle over this client's verified connection: the
// refresh of a saved bundle. A zero ifModifiedSince fetches
// unconditionally; on 304 it returns (nil, false, nil).
func (c *Client) CA(ctx context.Context, ifModifiedSince time.Time) (pem []byte, changed bool, err error) {
	return c.conditionalGet(ctx, "/puppet-ca/v1/certificate/ca", ifModifiedSince)
}

// CRL fetches the CRL bundle. A zero ifModifiedSince fetches unconditionally;
// on 304 it returns (nil, false, nil).
func (c *Client) CRL(ctx context.Context, ifModifiedSince time.Time) (pem []byte, changed bool, err error) {
	return c.conditionalGet(ctx, "/puppet-ca/v1/certificate_revocation_list/ca", ifModifiedSince)
}

func (c *Client) conditionalGet(ctx context.Context, path string, ifModifiedSince time.Time) ([]byte, bool, error) {
	var h http.Header
	if !ifModifiedSince.IsZero() {
		h = http.Header{"If-Modified-Since": {ifModifiedSince.UTC().Format(http.TimeFormat)}}
	}
	resp, err := c.do(ctx, http.MethodGet, path, nil, acceptText, "", nil, h)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, false, nil
	case http.StatusOK:
		b, err := readBody(resp, maxPEM)
		if err != nil {
			return nil, false, err
		}
		return b, true, nil
	}
	return nil, false, newHTTPError(resp)
}

// SubmitCSR submits the node's certificate signing request.
func (c *Client) SubmitCSR(ctx context.Context, certname string, csrPEM []byte) error {
	seg, err := pathSegment(certname)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPut, "/puppet-ca/v1/certificate_request/"+seg, nil, acceptText, "text/plain", csrPEM, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return newHTTPError(resp)
	}
	_, err = readBody(resp, maxErrorBody)
	return err
}

// Certificate fetches the signed certificate for certname. A 404 (not signed
// yet) is found=false with no error.
func (c *Client) Certificate(ctx context.Context, certname string) (pem []byte, found bool, err error) {
	seg, err := pathSegment(certname)
	if err != nil {
		return nil, false, err
	}
	resp, err := c.do(ctx, http.MethodGet, "/puppet-ca/v1/certificate/"+seg, nil, acceptText, "", nil, nil)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil, false, nil
	case http.StatusOK:
		b, err := readBody(resp, maxPEM)
		if err != nil {
			return nil, false, err
		}
		return b, true, nil
	}
	return nil, false, newHTTPError(resp)
}

// Node asks the server which environment certname belongs in.
func (c *Client) Node(ctx context.Context, certname, environment, transactionUUID string) (env string, err error) {
	seg, err := pathSegment(certname)
	if err != nil {
		return "", err
	}
	q := url.Values{
		"environment":            {environment},
		"configured_environment": {environment},
		"transaction_uuid":       {transactionUUID},
	}
	var node struct {
		Environment string `json:"environment"`
	}
	if err := c.getJSON(ctx, "/puppet/v3/node/"+seg, q, maxSmallJSON, &node); err != nil {
		return "", err
	}
	if node.Environment == "" {
		return "", fmt.Errorf("puppetapi: node %s: response has no environment", certname)
	}
	return node.Environment, nil
}

// CatalogResponse is what a catalog request yields besides an error. Body is
// the response bytes unchanged, nil unless the server answered 200. Compiler
// is the X-Puppet-Compiler-Name header, which Puppet logs as "Catalog compiled
// by <name>" whatever the status (http/service/compiler.rb). Peer is the IP
// address of the connection the request went out on, "" if none was made:
// Puppet names it in "Requesting catalog from" (indirector/catalog/rest.rb,
// which resolves the name again; the connection's address is the one used).
type CatalogResponse struct {
	Body           []byte
	Compiler, Peer string
}

// Catalog requests a compiled catalog. The response's Compiler and Peer are
// set as far as the request got, on error too.
//
// The form is Puppet's (puppet/http/service/compiler.rb):
// fields in its order, each value encoded with Puppet::Util.uri_query_encode
// (CGI.escape with spaces as %20), and facts encoded twice, so the server
// form-decodes and then percent-decodes once more. environment is sent both in
// the query and in the form. static_catalog is false: voxagent does not
// support inlined metadata.
func (c *Client) Catalog(ctx context.Context, certname string, form CatalogForm) (CatalogResponse, error) {
	var out CatalogResponse
	seg, err := pathSegment(certname)
	if err != nil {
		return out, err
	}
	fields := [][2]string{
		{"facts_format", "application/json"},
		{"facts", uriQueryEncode(string(form.Facts))},
		{"environment", form.Environment},
		{"configured_environment", form.Environment},
		{"check_environment", "true"},
		{"transaction_uuid", form.TransactionUUID},
		{"job_uuid", ""},
		{"static_catalog", "false"},
		{"checksum_type", checksumTypes},
	}
	var body strings.Builder
	for i, f := range fields {
		if i > 0 {
			body.WriteByte('&')
		}
		body.WriteString(f[0] + "=" + uriQueryEncode(f[1]))
	}
	q := url.Values{"environment": {form.Environment}}
	var (
		mu   sync.Mutex
		peer string
	)
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(ci httptrace.GotConnInfo) {
			if host, _, err := net.SplitHostPort(ci.Conn.RemoteAddr().String()); err == nil {
				mu.Lock()
				peer = host
				mu.Unlock()
			}
		},
	})
	resp, err := c.do(ctx, http.MethodPost, "/puppet/v3/catalog/"+seg, q, acceptCatalog,
		"application/x-www-form-urlencoded", []byte(body.String()), nil)
	mu.Lock()
	out.Peer = peer
	mu.Unlock()
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	out.Compiler = Sanitise(resp.Header.Get("X-Puppet-Compiler-Name"))
	if resp.StatusCode != http.StatusOK {
		return out, newHTTPError(resp)
	}
	out.Body, err = readBody(resp, maxCatalog)
	return out, err
}

// uriQueryEncode is Puppet::Util.uri_query_encode: CGI.escape, which leaves
// only ASCII letters, digits and "-_.~" unescaped (the same set as
// url.QueryEscape), with spaces as %20 rather than "+". A literal "+" is
// already %2B, so every "+" left is a space.
func uriQueryEncode(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// FileMetadata fetches the metadata of one file, as a File resource's
// puppet:/// source does.
func (c *Client) FileMetadata(ctx context.Context, mountPath, environment string, links string) (Metadata, error) {
	p, err := mountURLPath(mountPath)
	if err != nil {
		return Metadata{}, err
	}
	q := url.Values{
		"links":              {links},
		"checksum_type":      {"sha256"},
		"source_permissions": {"ignore"},
		"environment":        {environment},
	}
	var m Metadata
	if err := c.getJSON(ctx, "/puppet/v3/file_metadata/"+p, q, maxSmallJSON, &m); err != nil {
		return Metadata{}, err
	}
	return m, nil
}

// FileMetadatas fetches the metadata of a mount or directory tree.
func (c *Client) FileMetadatas(ctx context.Context, mountPath, environment string, p MetadatasParams) ([]Metadata, error) {
	mp, err := mountURLPath(mountPath)
	if err != nil {
		return nil, err
	}
	q := url.Values{
		"recurse":            {strconv.FormatBool(p.Recurse)},
		"links":              {p.Links},
		"checksum_type":      {"sha256"},
		"source_permissions": {p.SourcePermissions},
		"environment":        {environment},
	}
	if p.Recurse {
		q.Set("max_files", strconv.Itoa(p.MaxFiles))
		if len(p.Ignore) > 0 {
			q["ignore"] = p.Ignore
		}
	}
	var ms []Metadata
	if err := c.getJSON(ctx, "/puppet/v3/file_metadatas/"+mp, q, maxMetadatas, &ms); err != nil {
		return nil, err
	}
	return ms, nil
}

// FileContent streams a file's content, at most maxFileContent bytes, into w.
// Nothing is written to w unless the server answered 200; on a failure
// part-way, including a body over the cap, the caller must discard what was
// written. Integrity is the caller's: it hashes w against the metadata.
func (c *Client) FileContent(ctx context.Context, mountPath, environment string, w io.Writer) error {
	p, err := mountURLPath(mountPath)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodGet, "/puppet/v3/file_content/"+p, url.Values{"environment": {environment}}, acceptOctet, "", nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return newHTTPError(resp)
	}
	tooBig := fmt.Errorf("puppetapi: GET %s: response body exceeds %d bytes", resp.Request.URL.EscapedPath(), maxFileContent)
	if resp.ContentLength > maxFileContent {
		return tooBig
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, maxFileContent+1))
	if err != nil {
		return fmt.Errorf("puppetapi: GET %s: %w", resp.Request.URL.EscapedPath(), err)
	}
	if n > maxFileContent {
		return tooBig
	}
	return nil
}

// Report submits a run report (JSON, report_format 12).
func (c *Client) Report(ctx context.Context, certname, environment string, body []byte) error {
	seg, err := pathSegment(certname)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPut, "/puppet/v3/report/"+seg, url.Values{"environment": {environment}}, acceptJSON, "application/json", body, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return newHTTPError(resp)
	}
	_, err = readBody(resp, maxSmallJSON)
	return err
}

func (c *Client) getJSON(ctx context.Context, path string, q url.Values, limit int64, v any) error {
	resp, err := c.do(ctx, http.MethodGet, path, q, acceptJSON, "", nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return newHTTPError(resp)
	}
	b, err := readBody(resp, limit)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("puppetapi: GET %s: decode response: %w", resp.Request.URL.EscapedPath(), err)
	}
	return nil
}

type redactPathKey struct{}

// RedactPath returns ctx marking its requests' mount paths as secret: the HTTP
// debug line shows [redacted] in place of the path after /puppet/v3/<endpoint>/.
// It is for the requests of a puppet: source the catalog marked sensitive.
func RedactPath(ctx context.Context) context.Context {
	return context.WithValue(ctx, redactPathKey{}, true)
}

// redactMount replaces what follows the endpoint in a /puppet/v3/<endpoint>/
// path with [redacted].
func redactMount(escapedPath string) string {
	parts := strings.SplitN(escapedPath, "/", 5)
	if len(parts) < 5 {
		return escapedPath
	}
	return strings.Join(parts[:4], "/") + "/[redacted]"
}

// do sends one request with the headers Puppet sends on every request.
// escapedPath must already be escaped (pathSegment, mountURLPath).
func (c *Client) do(ctx context.Context, method, escapedPath string, q url.Values, accept, contentType string, body []byte, extra http.Header) (*http.Response, error) {
	u := c.base + escapedPath
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, fmt.Errorf("puppetapi: %w", err)
	}
	for k, vs := range extra {
		req.Header[k] = vs
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-Puppet-Version", report.PuppetVersion)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	started := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("puppetapi: %w", err)
	}
	// The path only: the query carries the environment and transaction UUID, and
	// no header or body is named.
	shown := escapedPath
	if ctx.Value(redactPathKey{}) != nil {
		shown = redactMount(escapedPath)
	}
	runlog.Debugf(ctx, "voxagent", "%s %s -> %d in %s", method, shown, resp.StatusCode,
		time.Since(started).Round(time.Millisecond))
	return resp, nil
}

// readBody reads the whole body, failing once it exceeds limit bytes.
func readBody(resp *http.Response, limit int64) ([]byte, error) {
	tooBig := fmt.Errorf("puppetapi: %s %s: response body exceeds %d bytes", resp.Request.Method, resp.Request.URL.EscapedPath(), limit)
	if resp.ContentLength > limit {
		return nil, tooBig
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("puppetapi: %s %s: read response: %w", resp.Request.Method, resp.Request.URL.EscapedPath(), err)
	}
	if int64(len(b)) > limit {
		return nil, tooBig
	}
	return b, nil
}

func newHTTPError(resp *http.Response) error {
	b, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	text := strings.TrimSpace(string(b))
	var puppetErr struct {
		Message *string `json:"message"`
	}
	if json.Unmarshal(b, &puppetErr) == nil && puppetErr.Message != nil {
		text = *puppetErr.Message
	}
	if readErr != nil {
		text += " (error body unreadable: " + readErr.Error() + ")"
	}
	return &HTTPError{
		Method:  resp.Request.Method,
		Path:    resp.Request.URL.EscapedPath(),
		Status:  resp.StatusCode,
		Message: Sanitise(text),
	}
}

// Sanitise makes untrusted text (a server's message, facter's stderr) safe to
// print or put in a report: control and format characters other than \n (terminal escapes, CR,
// bidi overrides) and invalid UTF-8 bytes are written as Go escapes, and the
// result is cut to maxMessage bytes, marked when it was cut.
func Sanitise(s string) string {
	const marker = " [truncated]"
	var b strings.Builder
	cut := -1 // length of b at the last point where b plus marker still fit
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		var piece string
		switch {
		case r == utf8.RuneError && size == 1:
			piece = fmt.Sprintf(`\x%02x`, s[i])
		case r == '\n':
			piece = "\n"
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			q := strconv.QuoteRuneToASCII(r)
			piece = q[1 : len(q)-1]
		default:
			piece = s[i : i+size]
		}
		i += size
		if b.Len()+len(piece) <= maxMessage-len(marker) {
			cut = b.Len() + len(piece)
		}
		b.WriteString(piece)
		if b.Len() > maxMessage {
			return b.String()[:cut] + marker
		}
	}
	return b.String()
}

// pathSegment path-escapes one URL path segment, refusing
// the values that would change the path's structure.
func pathSegment(s string) (string, error) {
	if s == "" || s == "." || s == ".." {
		return "", fmt.Errorf("puppetapi: %q is not a valid URL path segment", s)
	}
	return url.PathEscape(s), nil
}

// mountURLPath escapes a mount path such as "modules/vox_probe/x" segment by
// segment, keeping its slashes.
func mountURLPath(mountPath string) (string, error) {
	parts := strings.Split(mountPath, "/")
	for i, p := range parts {
		seg, err := pathSegment(p)
		if err != nil {
			return "", fmt.Errorf("puppetapi: mount path %q has an empty, \".\" or \"..\" segment", mountPath)
		}
		parts[i] = seg
	}
	return strings.Join(parts, "/"), nil
}
