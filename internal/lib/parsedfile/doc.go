// Package parsedfile ports Puppet's ParsedFile machinery for the native
// providers that edit a flat file as a list of records (cron first).
//
// This part is the FileParsing layer, a port of openvox 8.33.0's
// lib/puppet/util/fileparsing.rb, cited as "fp:<line>": a Grammar of line types
// turns text into Records and back. Its behaviour is pinned by
// testdata/vectors.json, whose wants come from running the real Ruby
// (testdata/fileparsing_vectors.rb) and which a Go test and a Ruby parity test
// both read.
//
// parsedfile.go is the ParsedFile layer, cited as "pf:<line>" (provider/parsedfile.rb):
// a Class describes a provider and a per-run Run holds its records, modified
// targets and backup marks.
//
// The package is a library: it knows nothing of catalogs or units. Patterns
// are Go regexp (RE2); never write \s or \S in one, as Ruby's \s includes \v
// and RE2's does not. Use Space and NotSpace.
package parsedfile
