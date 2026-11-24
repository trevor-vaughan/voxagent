package pkg

import (
	"regexp"
	"strings"
)

// multiVersionSeparator joins the versions of a package that is installed more
// than once, as rpm.rb:30's `MULTIVERSION_SEPARATOR = "; "` does.
//
// It is an OUTPUT-CONTRACT value, not a display choice. The joined string is
// what rpm.rb:105 puts in `@property_hash[:ensure]`, which is what the
// transaction reads back as an event's previous_value -- so a kernel installed
// twice reports previous_value="6.12.0-1; 6.12.0-2" verbatim. Changing the
// separator changes a reported value, and diverges from Ruby on the leaf.
const multiVersionSeparator = "; "

// nevraLine is rpm.rb:28's NEVRA_REGEX. Its fields are name, epoch, version,
// release and arch, separated by exactly one space; a leading quote is
// tolerated and dropped.
var nevraLine = regexp.MustCompile(`^'?(\S+) (\S+) (\S+) (\S+) (\S+)$`)

// parseInstalled parses rpm output in rpmQueryFormat -- rpm.rb's NEVRA_FORMAT
// -- into package name -> installed version.
//
// Each line is matched as rpm.rb:209 does, against NEVRA_REGEX after
// `line.strip!`, and the version is rebuilt as rpm.rb:213-214 does:
// `version-release`, with `epoch:` prefixed only when the epoch is not '0'.
// So `dbus 1 1.14.10 6.el10 x86_64` is "1:1.14.10-6.el10", and a package with
// no epoch and one built with an explicit `Epoch: 0` both read bare.
//
// An unreadable line is SKIPPED, not fatal and not a stopping point, matching
// rpm.rb:231's `next if hash.empty?`. One odd line in a real `rpm -qa` must not
// truncate the map the rest of the provider reasons from, and must not fail a
// run either: rpm.rb only debug-logs it (rpm.rb:215).
//
// UNRESOLVED DIVERGENCE, tracked as a prediction recorded when the apply spine
// was designed: rpm.rb's accumulator flushes whenever the name changes
// (:237-241), so it groups only ADJACENT same-name lines, and a package listed
// twice non-adjacently becomes two entries in Ruby's list.
// provider/package.rb:7-14's prefetch then assigns
// packages[prov.name] = prov, so the LAST of those entries wins. This map
// groups globally and would join every version instead.
//
// Left as-is deliberately, and NOT because the map is better. "Ruby reports
// only the last group" is a claim about what is in the output, read and not
// run, which is the form this project has got wrong three times; and choosing
// the nicer behaviour over the matching one is what the standing directive
// forbids. Measure it -- an rpm -qa whose duplicates are separated by another
// package -- then either match Ruby here or give it a divergence register row.
// Do not resolve it by reasoning.
func parseInstalled(out string) map[string]string {
	installed := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		// NEVRA_REGEX's anchoring is what rejects a blank line, a
		// whitespace-only line, and anything with too few fields, all without
		// a special case.
		m := nevraLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		name, epoch, version := m[1], m[2], m[3]+"-"+m[4]
		if epoch != "0" {
			version = epoch + ":" + version
		}

		existing, seen := installed[name]
		if !seen {
			installed[name] = version
			continue
		}
		// rpm.rb:244's `unless multiversion_hash[:ensure].include?(hash[:ensure])`.
		// Ruby's include? is substring containment on the ACCUMULATED string,
		// not equality against the last version, so this is Contains rather
		// than a comparison. Without it a package listed twice at one version
		// would report "6.12.0-1; 6.12.0-1", which no Ruby report contains.
		if strings.Contains(existing, version) {
			continue
		}
		installed[name] = existing + multiVersionSeparator + version
	}
	return installed
}

// checkUpdateVersion is provider/package/yum.rb:120's VERSION_REGEX, which
// splits a check-update version column into optional epoch, version and
// release. Both `\S+` are greedy, so the split is at the LAST hyphen.
var checkUpdateVersion = regexp.MustCompile(`^(?:([0-9]+):)?(\S+)-(\S+)$`)

// checkUpdateTrailer is yum.rb's `str.partition(/^(Obsoleting|Security:|Update)/)`.
// Everything from the first such line onwards is a trailing section listing
// packages that are NOT plain available updates, and is dropped.
var checkUpdateTrailer = regexp.MustCompile(`(?m)^(Obsoleting|Security:|Update)`)

// checkUpdateBlank is yum.rb's `body.split(/^\s*\n/)`: a run of blank or
// whitespace-only lines separates the output into chunks.
var checkUpdateBlank = regexp.MustCompile(`(?m)^\s*\n`)

// parseCheckUpdate parses `dnf -y check-update` output into the newest
// available version of each package, exactly as yum.rb's parse_updates does.
//
// The value is ALWAYS epoch-qualified -- `0:2.21-44.el10` for a package with no
// epoch at all -- because yum.rb's latest builds it as
// "#{upd[:epoch]}:#{upd[:version]}-#{upd[:release]}" from an epoch that
// update_to_hash defaults to "0". That asymmetry is deliberate and measured
// (AMENDMENT A3): a `<version>` sync carries no epoch on either side, so there
// is no shared version formatter to reuse here.
//
// Each package is indexed under BOTH its bare name and `name.arch`, because
// yum treats the two as interchangeable and a catalog may name either. The
// FIRST entry for a key wins, matching latest_package_version's `.first` on the
// array Ruby accumulates.
//
// Three details of the walk are load-bearing, and all three come from Ruby:
//
//   - Chunks are split on ALL whitespace and consumed in slices of three, not
//     line by line. The columns are space-padded and some rows carry trailing
//     whitespace, so a field walk is what the real capture in
//     testdata/dnf-check-update-updates.txt needs.
//   - A slice is an update only if its first field contains a "." and its
//     second parses as a version. That filter is why Ruby can leave `-y` on the
//     command line: with a cold cache dnf writes repo download progress to
//     stdout, and no slice of it survives both tests.
//   - The package name is the first field up to its LAST ".", not its first:
//     `gdb-gdbserver.x86_64` is gdb-gdbserver, and cutting at the first dot
//     would mis-parse every package whose name contains one.
func parseCheckUpdate(out string) map[string]string {
	if trailer := checkUpdateTrailer.FindStringIndex(out); trailer != nil {
		out = out[:trailer[0]]
	}
	updates := make(map[string]string)
	for _, chunk := range checkUpdateBlank.Split(out, -1) {
		fields := strings.Fields(chunk)
		for i := 0; i+1 < len(fields); i += 3 {
			pkg, version := fields[i], fields[i+1]
			name, arch, dotted := lastCut(pkg, ".")
			// Ruby raises "Failed to parse package name and architecture"
			// for an empty name rather than skipping it. That needs a first
			// field of the form ".x" -- one dot, at the start -- which dnf's
			// fixed three-column output cannot produce, and which the version
			// test below would have to pass as well. Skipping is not a policy
			// choice about a reachable input; it is the same non-event.
			if !dotted || name == "" {
				continue
			}
			evr := checkUpdateVersion.FindStringSubmatch(version)
			if evr == nil {
				continue
			}
			epoch := evr[1]
			if epoch == "" {
				epoch = "0"
			}
			newest := epoch + ":" + evr[2] + "-" + evr[3]
			for _, key := range [2]string{name, name + "." + arch} {
				if _, seen := updates[key]; !seen {
					updates[key] = newest
				}
			}
		}
	}
	return updates
}

// lastCut is strings.Cut at the LAST separator rather than the first, which is
// Ruby's String#rpartition. strings has no such function.
func lastCut(s, sep string) (before, after string, found bool) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}
