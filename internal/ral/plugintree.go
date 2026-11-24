package ral

// PluginTree is the plugin tree the concat unit and the augeas routes fingerprint:
// the libdir the extension host would load types from. It is an interface so
// tests do not depend on the machine they run on.
type PluginTree interface {
	// Read returns the file at rel, a slash-separated path under the tree,
	// following symlinks; a file over 1 MiB is an error.
	Read(rel string) ([]byte, error)
	// List returns the names of the entries of the directory at rel, a
	// slash-separated path under the tree, following symlinks.
	List(rel string) ([]string, error)
}
