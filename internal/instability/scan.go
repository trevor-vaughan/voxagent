// Package instability flags source constructs that break the determinism the
// catalog field allowlist depends on.
//
// The allowlist holds that two compiles of identical inputs differ only in
// version and catalog_uuid. That is true for the default path. It is not true
// when compilation reaches outside itself: exported resources round-trip
// through PuppetDB, generate() shells out at compile time, and a custom
// function doing I/O can return anything.
//
// Detection is line-oriented and deliberately conservative. It is a signal for
// a human to investigate, not a proof.
package instability

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

// Finding kinds.
const (
	KindExportedResource  = "exported_resource"
	KindExportedCollector = "exported_collector"
	KindGenerateCall      = "generate_call"
	KindFunctionIO        = "function_io"
)

// Finding is one flagged construct.
type Finding struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Kind    string `json:"kind"`
	Excerpt string `json:"excerpt"`
}

var (
	exportedResourceRE  = regexp.MustCompile(`@@[a-z_][\w:]*\s*\{`)
	exportedCollectorRE = regexp.MustCompile(`<<\|.*\|>>`)
	generateCallRE      = regexp.MustCompile(`\bgenerate\s*\(`)

	functionIORE = regexp.MustCompile(
		`\b(File|IO|Dir|Open3|Net::HTTP|Socket|Kernel)\.|` +
			`\b(system|exec|spawn)\s*\(|` +
			"`|" +
			`%x\{`)
)

// ScanManifest flags non-deterministic constructs in a .pp manifest.
//
// It returns an error if src could not be fully scanned — most notably when a
// single line exceeds bufio.Scanner's 1 MiB buffer, in which case scanning
// stops mid-file. Returning findings gathered before a truncation error would
// silently understate the file's real instability, so the caller must not
// treat a non-nil error as advisory.
func ScanManifest(path string, src []byte) ([]Finding, error) {
	return scan(path, src, func(line string) string {
		switch {
		case exportedResourceRE.MatchString(line):
			return KindExportedResource
		case exportedCollectorRE.MatchString(line):
			return KindExportedCollector
		case generateCallRE.MatchString(line):
			return KindGenerateCall
		}
		return ""
	})
}

// ScanRubyFunction flags I/O in a custom Puppet function. See ScanManifest for
// the error contract.
func ScanRubyFunction(path string, src []byte) ([]Finding, error) {
	return scan(path, src, func(line string) string {
		if functionIORE.MatchString(line) {
			return KindFunctionIO
		}
		return ""
	})
}

// scan walks src line by line, skipping blank and comment lines, and calls
// classify on each remaining line. Both the Puppet language and Ruby use "#"
// for comments, so the prefix is fixed rather than a parameter.
func scan(path string, src []byte, classify func(string) string) ([]Finding, error) {
	var out []Finding
	sc := bufio.NewScanner(bytes.NewReader(src))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if kind := classify(line); kind != "" {
			out = append(out, Finding{File: path, Line: n, Kind: kind, Excerpt: trimmed})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}
	return out, nil
}
