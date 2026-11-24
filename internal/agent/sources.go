package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/puppetapi"
	"github.com/voxagent/voxagent/internal/runlog"
)

// sourceFetch is one distinct puppet: source value of the catalog.
type sourceFetch struct {
	uri       string // as the catalog wrote it: the key of the returned map
	mountPath string // e.g. "modules/m/f"
	recurse   bool   // some File with this source has recurse => true
	links     string // the File's links: "manage" or "follow"
	marked    bool   // some File with this source marks it sensitive (D-78)
}

// FetchSources downloads every puppet: source of every File in cat into dir
// and returns the map for ral.File.Sources: each source value, exactly as the
// catalog wrote it (every element of a list), to its local copy.
//
// dir is run-private: the caller makes it with os.MkdirTemp under vardir
// (0700) and removes it with os.RemoveAll on every exit path.
// Every write goes through an os.Root on dir; directories are
// 0700 and files 0600, set explicitly. The i-th distinct source (catalog
// order) is dir/<i>:
//
//   - a file source is a file there, its content verified against the
//     metadata checksum while it streams, as pluginsync's are;
//   - a source of a File with recurse => true is a directory mirroring the
//     server's tree, every relative_path checked by validRelPath;
//   - a source the server answers 404 for is a path that does not exist, so
//     the provider's own "Could not retrieve information from environment …"
//     message names it, as Puppet's does.
//
// server is --server. A puppet://<host>/ source whose host is not server, or
// that names a port or user, is an error, checked for every source
// before any request. So is link metadata: with links => manage,
// Puppet would make the destination a symlink, which ral does not implement,
// and nothing here creates one. Requests are Puppet's:
// file_metadata then file_content for a file, file_metadatas with
// recurse=true and max_files=0 for a recursed source; content is fetched on
// every run, not only when the destination's checksum differs.
func FetchSources(ctx context.Context, c *puppetapi.Client, cat *catalog.Catalog, environment, server, dir string) (map[string]string, error) {
	fetches, err := collectSources(cat, server)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("sources: %w", err)
	}
	defer root.Close()

	m := make(map[string]string, len(fetches))
	for i, f := range fetches {
		name := strconv.Itoa(i)
		started := time.Now()
		fctx, shown := ctx, f.uri
		if f.marked {
			fctx, shown = puppetapi.RedactPath(ctx), "[redacted]"
		}
		var n int64
		if f.recurse {
			n, err = fetchTree(fctx, c, environment, root, name, f)
		} else {
			n, err = fetchOne(fctx, c, environment, root, name, f)
		}
		if err != nil {
			return nil, fmt.Errorf("source %s: %w", f.uri, err)
		}
		runlog.Debugf(ctx, "voxagent", "source %s: %d bytes in %s", shown, n, time.Since(started).Round(time.Millisecond))
		m[f.uri] = filepath.Join(dir, name)
	}
	return m, nil
}

// collectSources lists the distinct puppet: source values of cat's Files, in
// catalog order, refusing any that would be fetched from somewhere other than
// server. A value that is not a string, not a puppet: URI or does not parse is
// left to ral, which refuses or reads it in Puppet's words.
func collectSources(cat *catalog.Catalog, server string) ([]sourceFetch, error) {
	var fetches []sourceFetch
	index := map[string]int{}
	for _, r := range cat.Resources {
		raw, ok := r.Parameters["source"]
		if r.Type != "File" || !ok {
			continue
		}
		values, ok := raw.([]any)
		if !ok {
			values = []any{raw}
		}
		for _, v := range values {
			s, ok := v.(string)
			if !ok {
				continue
			}
			mountPath, isPuppet, err := puppetMountPath(s, server)
			if err != nil {
				return nil, fmt.Errorf("%s: source %s: %w", r.Ref(), s, err)
			}
			if !isPuppet {
				continue
			}
			links := "manage"
			if v, ok := r.Parameters["links"]; ok {
				links, _ = v.(string)
				if links != "manage" && links != "follow" {
					return nil, fmt.Errorf("%s: source %s: links => %v is not implemented", r.Ref(), s, v)
				}
			}
			recurse := fmt.Sprint(r.Parameters["recurse"]) == "true"
			marked := slices.Contains(r.SensitiveParameters, "source")
			if i, seen := index[s]; seen {
				// One download serves every File with this source: a tree
				// also serves a File that does not recurse, but links
				// changes what the server sends.
				if fetches[i].links != links {
					return nil, fmt.Errorf("%s: source %s: links => %s, but another File with this source has links => %s", r.Ref(), s, links, fetches[i].links)
				}
				fetches[i].recurse = fetches[i].recurse || recurse
				fetches[i].marked = fetches[i].marked || marked
				continue
			}
			index[s] = len(fetches)
			fetches = append(fetches, sourceFetch{uri: s, mountPath: mountPath, recurse: recurse, links: links, marked: marked})
		}
	}
	return fetches, nil
}

// puppetMountPath returns the mount path a puppet: source names on server, and
// whether s is a puppet: URI at all. It parses s as ral's parseSourceURL does,
// so the path is the literal text: % is escaped first (Request#set_uri_key).
// A trailing "/" is not part of the mount path (source.rb strips it).
func puppetMountPath(s, server string) (string, bool, error) {
	u, err := url.Parse(strings.ReplaceAll(s, "%", "%25"))
	if err != nil || u.Scheme != "puppet" || u.Opaque != "" {
		return "", false, nil
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", true, errors.New("a puppet: source with a user, query or fragment is not implemented")
	}
	if u.Host != "" && !strings.EqualFold(u.Host, server) {
		return "", true, fmt.Errorf("names host %q; sources are fetched only from the server %s", u.Host, server)
	}
	mountPath := strings.TrimRight(strings.TrimPrefix(u.Path, "/"), "/")
	if err := validRelPath(mountPath); err != nil {
		return "", true, fmt.Errorf("not a mount path: %w", err)
	}
	return mountPath, true, nil
}

// fetchOne downloads a non-recursed source to name in root and returns the
// content bytes it downloaded.
func fetchOne(ctx context.Context, c *puppetapi.Client, environment string, root *os.Root, name string, f sourceFetch) (int64, error) {
	md, err := c.FileMetadata(ctx, f.mountPath, environment, f.links)
	if he := (*puppetapi.HTTPError)(nil); errors.As(err, &he) && he.Status == http.StatusNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	switch md.Type {
	case "file":
		return fetchFile(ctx, c, environment, f.mountPath, root, name, md)
	case "directory":
		return 0, mkdirPrivate(root, name)
	case "link":
		return 0, linkRefused(".", f.links)
	default:
		return 0, fmt.Errorf("unknown type %q", md.Type)
	}
}

// fetchTree downloads a recursed source to name in root: the tree under a
// directory, or the file itself. It returns the content bytes downloaded.
func fetchTree(ctx context.Context, c *puppetapi.Client, environment string, root *os.Root, name string, f sourceFetch) (int64, error) {
	ms, err := c.FileMetadatas(ctx, f.mountPath, environment, puppetapi.MetadatasParams{
		Recurse: true, MaxFiles: 0, Links: f.links, SourcePermissions: "ignore",
	})
	if he := (*puppetapi.HTTPError)(nil); errors.As(err, &he) && he.Status == http.StatusNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	for _, m := range ms {
		if m.Type == "link" {
			return 0, linkRefused(m.RelativePath, f.links)
		}
	}
	if len(ms) == 1 && ms[0].RelativePath == "." && ms[0].Type == "file" {
		return fetchFile(ctx, c, environment, f.mountPath, root, name, ms[0])
	}
	entries, err := validateMetadata(ms, false)
	if err != nil {
		return 0, err
	}
	if err := mkdirPrivate(root, name); err != nil {
		return 0, err
	}
	// Sorted, a directory comes before everything under it.
	for _, e := range entries {
		if e.dir {
			if err := mkdirPrivate(root, name+"/"+e.rel); err != nil {
				return 0, err
			}
		}
	}
	var total int64
	for _, e := range entries {
		if e.dir {
			continue
		}
		e.mode = 0o600
		rel := e.rel
		e.rel = name + "/" + rel
		n, _, err := syncFile(ctx, c, environment, f.mountPath+"/"+rel, root, e)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", rel, err)
		}
		total += n
	}
	return total, nil
}

// fetchFile downloads the file md describes, at mountPath, to name in root.
func fetchFile(ctx context.Context, c *puppetapi.Client, environment, mountPath string, root *os.Root, name string, md puppetapi.Metadata) (int64, error) {
	typ, digest, err := parseChecksum(md.Checksum.Type, md.Checksum.Value)
	if err != nil {
		return 0, err
	}
	n, _, err := syncFile(ctx, c, environment, mountPath, root, entry{rel: name, mode: 0o600, hash: typ, digest: digest})
	return n, err
}

// mkdirPrivate makes rel in root a new directory with mode 0700, independent
// of the umask.
func mkdirPrivate(root *os.Root, rel string) error {
	if err := root.Mkdir(rel, 0o700); err != nil {
		return err
	}
	return root.Chmod(rel, 0o700)
}

func linkRefused(rel, links string) error {
	return fmt.Errorf("relative_path %q is a link (links => %s): Puppet would create a symlink, which is not implemented, and none is created from server metadata", rel, links)
}
