package file

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

// parseSourceURL parses a source the way Puppet does: % is escaped first
// (Request#set_uri_key's uri_encode) and unescaped once when the path is
// taken (uri_to_path), so the returned Path is the literal text and a "%zz" is
// no parse error. Measured on Puppet 8.10.0: file:///d/a%20b/f reads the
// directory named "a%20b", not "a b".
func parseSourceURL(s string) (*url.URL, error) {
	u, err := url.Parse(strings.ReplaceAll(s, "%", "%25"))
	if err != nil {
		// Report the text the catalog gave, not the escaped copy.
		var ue *url.Error
		if errors.As(err, &ue) {
			ue.URL = s
		}
		return nil, err
	}
	return u, nil
}

// supportedSource reports whether voxagent can read a source value: an
// absolute path, a file: URI, or a puppet: URI the agent downloaded, which is
// a key of sources exactly as the catalog wrote it (File.Sources). A value
// with no scheme is accepted here so that Validate refuses it in Puppet's
// words ("Cannot use relative URLs"): it is invalid in Puppet too, not a
// porting gap. Every other value -- an unmapped puppet:, http:, https: --
// needs a transport voxagent does not have.
func supportedSource(value string, sources map[string]string) bool {
	if filepath.IsAbs(value) {
		return true
	}
	u, err := parseSourceURL(value)
	if err != nil || u.Scheme == "" {
		return true
	}
	if u.Scheme == "puppet" {
		_, mapped := sources[value]
		return mapped
	}
	return u.Scheme == "file"
}

// validateSource applies type/file/source.rb:86-103 to every element of a
// source value, in Puppet's order, plus one rule of voxagent's own: a file:
// URI names no host. A parameter's validation failure reaches the run as
// type.rb:692's "Parameter source failed on <ref>: <detail>".
//
// redact shows the source as [redacted], quotes included, in the failures: a
// marked source stands for the file's content and ensure, and Ruby leaks it
// in its error text (D-78).
func validateSource(ref string, raw any, redact bool) error {
	values, ok := raw.([]any)
	if !ok {
		values = []any{raw}
	}
	for _, v := range values {
		s, ok := v.(string)
		if !ok {
			s = fmt.Sprint(v)
		}
		if filepath.IsAbs(s) {
			continue
		}
		u, err := parseSourceURL(s)
		switch {
		case err != nil:
			if redact {
				return fmt.Errorf("Parameter source failed on %s: Could not understand source [redacted]", ref)
			}
			return fmt.Errorf("Parameter source failed on %s: Could not understand source %s: %v", ref, s, err)
		case u.Scheme == "":
			return fmt.Errorf("Parameter source failed on %s: Cannot use relative URLs %s", ref, rubyvalue.RedactIf(redact, "'%s'", s))
		case u.Opaque != "":
			return fmt.Errorf("Parameter source failed on %s: Cannot use opaque URLs %s", ref, rubyvalue.RedactIf(redact, "'%s'", s))
		case u.Scheme == "file" && u.Host != "":
			// Ours: a host names another machine, which voxagent cannot read.
			return fmt.Errorf("Parameter source failed on %s: file: URI %s names host %s; only a local file: URI is implemented",
				ref, rubyvalue.RedactIf(redact, "'%s'", s), rubyvalue.RedactIf(redact, "'%s'", u.Host))
		}
	}
	return nil
}

// sourceSpec is one candidate source.
type sourceSpec struct {
	// path is the local path voxagent reads.
	path string
	// shown is the value Puppet's messages print. source.rb:107-128 strips
	// trailing separators, then turns an absolute path into a file URI and
	// unescapes it again, so /srv/a/ reads file:///srv/a (measured: three
	// slashes). A URI, file: or puppet:, is only stripped.
	shown string
	// mapped is a puppet: source whose path is the agent's download of it.
	mapped bool
}

// sourceSpecs normalises a source value, a string or a list, in order. A value
// that is a key of sources (File.Sources) is read from the path it maps to.
// Validate has refused every value this cannot read; a URI that does not parse
// is still returned, with no path, so it reads as missing rather than
// vanishing from the message.
func sourceSpecs(raw any, sources map[string]string) []sourceSpec {
	values, ok := raw.([]any)
	if !ok {
		values = []any{raw}
	}
	specs := make([]sourceSpec, 0, len(values))
	for _, v := range values {
		s, ok := v.(string)
		if !ok {
			s = fmt.Sprint(v)
		}
		trimmed := strings.TrimRight(s, "/")
		if local, ok := sources[s]; ok {
			specs = append(specs, sourceSpec{path: local, shown: trimmed, mapped: true})
			continue
		}
		if filepath.IsAbs(trimmed) {
			specs = append(specs, sourceSpec{path: trimmed, shown: "file://" + trimmed})
			continue
		}
		path := ""
		if u, err := parseSourceURL(trimmed); err == nil {
			path = u.Path
		}
		specs = append(specs, sourceSpec{path: path, shown: trimmed})
	}
	return specs
}

// chooseSource returns the first candidate that exists, with its type read as
// follow says, and whether one did. On an error it returns the candidate that caused it.
//
// Existence is Puppet's File.exist? (indirector/direct_file_server.rb:10,16):
// it follows links and is false for any stat failure, so a dangling symlink,
// or a path behind a directory this process cannot search, is a missing
// candidate and the next is tried (source.rb:191-204). What is described is
// the candidate itself, not a link's target, under links => manage, the
// default, which collects metadata with lstat (file_serving/base.rb:79);
// links => follow collects it with stat.
//
// fsys is osFS for a catalog resource's source and a rootFS on the source
// directory for a generated child's (childSourceFS).
func chooseSource(fsys fileSystem, specs []sourceSpec, follow bool) (sourceSpec, os.FileInfo, bool, error) {
	for _, s := range specs {
		if _, err := fsys.Stat(s.path); err != nil {
			continue
		}
		fi, err := statFor(fsys, s.path, follow)
		if err != nil {
			return s, nil, false, err
		}
		return s, fi, true, nil
	}
	return sourceSpec{}, nil, false, nil
}

// hideDownload names a mapped puppet: source in err as the catalog wrote it
// rather than by the run-private path of its download, which Puppet's messages
// never contain. Any other source's error is returned as it is.
// For a marked source it takes the source's path out of err (D-78).
func hideDownload(spec sourceSpec, err error, redact bool) error {
	switch {
	case redact:
		return errors.New(strings.ReplaceAll(err.Error(), spec.path, "[redacted]"))
	case spec.mapped:
		return errors.New(strings.ReplaceAll(err.Error(), spec.path, spec.shown))
	}
	return err
}

// sourceMarked reports whether r's source is marked sensitive. It reads the
// catalog's list, not MarkSensitive's set: a marked source is replaced there
// by the content and ensure it stands for. A generated child carries no marks
// of its own, but its errors name the source its marked parent walked, so the
// parent's mark counts.
func sourceMarked(r catalog.Resource) bool {
	if slices.Contains(r.SensitiveParameters, "source") {
		return true
	}
	return r.GeneratedFrom != nil && sourceMarked(*r.GeneratedFrom)
}

// resolvedSource is what a resource's source says its path must be.
type resolvedSource struct {
	spec  sourceSpec
	isDir bool
	// linkTarget is the raw readlink of a symlink source under links =>
	// manage, which makes the resource a link to it; empty otherwise.
	linkTarget string
	// follow is links => follow: the source is read through a final symlink.
	follow bool
	// sum is a regular file's checksum when it was resolved: Puppet's
	// metadata checksum, which the written copy is checked against
	// (type/file.rb:1076-1100).
	sum string
	// fsys is what the source was resolved through, and what its content is
	// read through.
	fsys fileSystem
	// redact shows the source as [redacted] in errors: the source is marked.
	redact bool
}

// resolveSource finds and describes r's source, reading through fsys, with
// sources mapping each downloaded puppet: source to its local copy.
func resolveSource(fsys fileSystem, r catalog.Resource, environment string, sources map[string]string) (resolvedSource, error) {
	specs := sourceSpecs(r.Parameters["source"], sources)
	follow := linksFollow(r)
	spec, fi, found, err := chooseSource(fsys, specs, follow)
	// A marked source is shown as [redacted] in every error (D-78: Ruby leaks
	// it).
	redact := sourceMarked(r)
	if err != nil {
		return resolvedSource{}, fmt.Errorf("Could not retrieve file metadata for %s: %w",
			rubyvalue.RedactIf(redact, "%s", spec.shown), hideDownload(spec, err, redact))
	}
	if !found {
		shown := make([]string, len(specs))
		for i, s := range specs {
			shown[i] = rubyvalue.RedactIf(redact, "%s", s.shown)
		}
		// source.rb:208, verbatim. Measured on Puppet 8.10.0: "Could not
		// retrieve information from environment production source(s)
		// file:///tmp/pm/nonexistent" -- a resource failure with no file/line
		// suffix, whose dependents are skipped.
		return resolvedSource{}, fmt.Errorf("Could not retrieve information from environment %s source(s) %s",
			environment, strings.Join(shown, ", "))
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		// A link source means ensure => link to its raw target
		// (source.rb:170-176, file_serving/metadata.rb:118-124). Only manage
		// reaches here: follow's stat never answers a link.
		target, err := fsys.Readlink(spec.path)
		if err != nil {
			return resolvedSource{}, fmt.Errorf("Could not retrieve file metadata for %s: %w",
				rubyvalue.RedactIf(redact, "%s", spec.shown), hideDownload(spec, err, redact))
		}
		return resolvedSource{spec: spec, linkTarget: target, fsys: fsys, redact: redact}, nil
	case fi.IsDir():
		return resolvedSource{spec: spec, isDir: true, follow: follow, fsys: fsys, redact: redact}, nil
	case fi.Mode().IsRegular():
		f, err := openSource(fsys, spec.path, follow)
		if err != nil {
			return resolvedSource{}, fmt.Errorf("Could not retrieve file metadata for %s: %w",
				rubyvalue.RedactIf(redact, "%s", spec.shown), hideDownload(spec, err, redact))
		}
		sum, err := checksumOf(f)
		_ = f.Close()
		if err != nil {
			return resolvedSource{}, fmt.Errorf("Could not retrieve file metadata for %s: %w",
				rubyvalue.RedactIf(redact, "%s", spec.shown), hideDownload(spec, err, redact))
		}
		return resolvedSource{spec: spec, sum: sum, follow: follow, fsys: fsys, redact: redact}, nil
	default:
		// D-26: a FIFO, socket or device.
		return resolvedSource{}, fmt.Errorf("source %s is not a regular file or a directory; only those can be a source",
			rubyvalue.RedactIf(redact, "%s", spec.shown))
	}
}

// openSource opens a regular-file source and refuses whatever else it finds there once open: the
// source was Lstat'd when it was resolved, and a link or FIFO swapped in
// since must not be read in its place. Under links => manage the open does
// not follow a symlink at the final component (O_NOFOLLOW), so a link swapped
// in is an error; under follow it does, and the descriptor, not the path, is
// what is checked. O_NONBLOCK keeps a FIFO from blocking
// the open; it changes nothing for a regular file. A device node swapped in
// since is opened before the type check refuses it, and opening some devices
// has side effects of its own (a tape rewinds, a modem line is claimed);
// nothing reads or writes it. O_NOCTTY keeps a terminal swapped in from
// becoming the agent's controlling terminal.
//
// os.Root ignores O_NOFOLLOW and follows a final link that stays inside the
// root, so through a rootFS a link swapped in since resolution is read only
// if it points at a regular file in the same source tree.
func openSource(fsys fileSystem, path string, follow bool) (*os.File, error) {
	flags := os.O_RDONLY | syscall.O_NONBLOCK | syscall.O_NOCTTY
	if !follow {
		flags |= syscall.O_NOFOLLOW
	}
	f, err := fsys.OpenFile(path, flags, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s is no longer a regular file", path)
	}
	return f, nil
}

// sourceContent streams a resolved regular-file source into a write, hashing
// what it copies so the copy can be checked against the checksum taken at
// resolution.
func sourceContent(src resolvedSource) content {
	return content{sum: src.sum, staged: true, write: func(w io.Writer) (string, error) {
		f, err := openSource(src.fsys, src.spec.path, src.follow)
		if err != nil {
			return "", hideDownload(src.spec, err, src.redact)
		}
		defer f.Close()
		return checksumOf(io.TeeReader(f, w))
	}}
}
