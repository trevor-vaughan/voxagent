package ral

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/runlog"
)

// rpmQueryFormat is provider/package/rpm.rb:27's NEVRA_FORMAT, verbatim: five
// fields, the epoch printed as 0 when the package has none. parseInstalled
// rebuilds the version from them exactly as rpm.rb:213-214 does.
//
// It is Ruby's format rather than a shorter one that inlines the epoch, because
// the shorter one cannot express Ruby's rule. `%|EPOCH?{%{EPOCH}:}|` tests
// whether the epoch tag EXISTS, where Ruby drops an epoch EQUAL to '0', so a
// package built with an explicit `Epoch: 0` read "0:1.0-1" where Ruby reads
// "1.0-1". Measured with a locally built rpm.
const rpmQueryFormat = `%{NAME} %|EPOCH?{%{EPOCH}}:{0}| %{VERSION} %{RELEASE} %{ARCH}\n`

// notRootMessage is provider/package/yum.rb:73's refusal, verbatim. Measured in
// the captured report-nonroot.json as the tail of "Could not prefetch package
// provider 'dnf': The yum provider can only be used as root", where the
// transaction wiring supplies the prefix.
const notRootMessage = "The yum provider can only be used as root"

// Package is the Tier 1 package provider, bounded to dnf and to
// ensure/name/provider.
//
// Puppet ships more than thirty package providers and selects one at runtime
// from facts and command availability. This provider implements dnf only, and a
// catalog naming another provider is a pre-flight refusal rather than a runtime
// failure. That is the same bounded-surface discipline File applies to
// attributes, applied one level up, and it is what keeps Puppet's "failed with
// zero events, one err per type at teardown" shape (transaction.rb:132-135,
// :146) out of the contract.
//
// The zero value is what main registers; it can answer every question
// pre-flight asks, and Prefetch turns it into the value that can apply.
type Package struct {
	// run is the command seam. The zero value uses the real one.
	run commandRunner

	// dnfPath and rpmPath are the two commands Ruby's dnf provider declares
	// (dnf.rb:16, `commands :cmd => "dnf", :rpm => "rpm"`), resolved to
	// absolute paths by Prefetch.
	//
	// Absolute and resolved ONCE, both for the same reason: Puppet's `command`
	// resolves a declared command to its absolute path, and that path is quoted
	// verbatim inside the failure message the report carries -- measured,
	// "Execution of '/usr/bin/dnf -y install ...' returned 1: ...". A bare
	// "dnf" would diverge on every failed package.
	dnfPath string
	rpmPath string

	// state is the installed set, from Prefetch. A nil map means Prefetch has
	// not run; Apply treats that as an error rather than as "nothing is
	// installed", because the two are indistinguishable from the map alone and
	// guessing wrong would report every package as absent and install the lot.
	state map[string]string

	// updates is the newest available version of every package `dnf
	// check-update` named, keyed under BOTH the bare name and name.arch, and
	// resolved ONCE per run by Prefetch. Nil is the ordinary case: no resource
	// asked for `latest`, or dnf reported no updates, or check-update failed.
	//
	// All three collapse to the same behaviour, which is Ruby's: yum.rb's
	// `latest` falls back to the currently installed version whenever the map
	// has no entry, so the comparison is the version against itself and the
	// resource is in sync. A missing entry is never an error.
	updates map[string]string

	// checkUpdateFailure is yum.rb:129's warning text when check-update exited
	// anything other than 0 or 100, and empty otherwise. Carried rather than
	// logged because Prefetch has no warning channel -- see resolveUpdates for
	// why the resolve lives there at all.
	checkUpdateFailure string
}

// Type implements Provider.
func (Package) Type() string { return "package" }

// dnfProvider is the provider name every captured package resource_status
// carries. A package-level variable because ProviderUsed returns a pointer; see
// posix, which exists for the same reason.
var dnfProvider = "dnf"

// dnfProviderSource is what a warning raised by this provider's CLASS, rather
// than by a resource, is sourced as. util/logging.rb:298-302's log_source finds
// neither a Type nor a Parameter on a class-level call and falls through to the
// class's own rendering. Measured verbatim:
//
//	warning | Puppet::Type::Package::ProviderDnf | Could not check for updates, ...
const dnfProviderSource = "Puppet::Type::Package::ProviderDnf"

// checkUpdateWarning is provider/package/yum.rb:129's wording. The command is
// interpolated from `command(:cmd)`, which Puppet resolves to an ABSOLUTE path
// (AMENDMENT A4) -- and the message names `check-update` alone, without the
// `-y` the argv actually carries.
const checkUpdateWarning = "Could not check for updates, '%s check-update' exited with %d"

// couldNotUpdate is the prefix type/package.rb:117-133's newvalue(:latest)
// rescue adds to a failed sync, via `self.fail Puppet::Error, _("Could not
// update: %{detail}")`. newvalue(:present) and newvalue(:absent) have no such
// rescue, so the same ExecutionFailure is reported with this prefix on the
// `latest` branch and without it on the other two. Measured both ways; see
// AMENDMENT A16, which also records that the <version> branch needs it.
const couldNotUpdate = "Could not update: "

// ProviderUsed implements Provider.
func (Package) ProviderUsed() *string { return &dnfProvider }

// AutoRequires implements Provider. type/package.rb declares no autorequire.
func (Package) AutoRequires(catalog.Resource, func(string) bool) []string { return nil }

// Attrs implements Provider.
//
// Three attributes, and nothing this provider does not read: source,
// install_options, uninstall_options, mark, flavor, package_settings,
// allow_virtual and the rest are refused at pre-flight by their absence.
// Declaring one would promise to honour it.
func (Package) Attrs() Attrs { return NewAttrs("ensure", "name", "provider") }

// Properties implements PropertyDeclaring. Of the three attributes only ensure
// is a property of Puppet's package type (type/package.rb ensurable); name and
// provider are parameters.
func (Package) Properties() Attrs { return NewAttrs("ensure") }

// purgedValue is the ensure value provider/package.rb's properties substitutes
// for a package rpm does not know. It is a REPORTED value -- every captured
// install event carries previous_value "purged", never "absent" -- and it is
// also what the message rule keys on, exactly as Ruby's does: type/package.rb:231
// returns "created" when the current value is nil, :absent or :purged.
const purgedValue = "purged"

// ensureIsVersion reports whether an ensure value names a concrete version.
//
// type/package.rb declares its symbolic values first and a catch-all
// newvalue(/./) last (:136), so Ruby's own classification is "anything the
// symbols did not claim is a version" -- a finite list of accepted values
// cannot express that, which is why this is a predicate rather than a set.
//
// The symbols below are the ones Puppet declares -- present (:99), absent
// (:103), purged (:107), disabled (:111), installed (:116) and latest (:118) --
// plus `held`, which Puppet does NOT declare: Ruby's catch-all would read it as
// a version and hand dnf `nano-held` to reject. This provider refuses it by
// name instead, and it has to be excluded here or the classification would
// promote it to a version and this provider would hand dnf the same nonsense.
func ensureIsVersion(ensure string) bool {
	switch ensure {
	case "", "present", "installed", "absent", "purged", "disabled", "latest", "held":
		return false
	case "true", "false":
		// A JSON boolean, rendered by paramString. Read as a version it would
		// install `nano-false`; it names no version, so it is refused.
		return false
	}
	return !isVersionRange(ensure)
}

// isVersionRange reports whether a value is a version RANGE rather than a
// concrete version.
//
// dnf.rb:14 declares the :version_ranges feature, so Ruby accepts `>=8.1` and
// the space-separated intersection `>8.1 <9.0`, resolving them through
// yum.rb's best_version -- which shells out to `dnf list --showduplicates` and
// picks the greatest available version inside the range. This provider does
// not implement that, so a range must be refused at PRE-FLIGHT.
//
// Refusing it here rather than letting ensureIsVersion call it a version is the
// whole point of the predicate. Classified as a version, `>=8.1` would pass
// pre-flight and then hand dnf `nano->=8.1` at apply -- the rest of the catalog
// applied and the node half-converged, which is exactly what ValueBounded
// exists to stop.
//
// The test is a leading OPERATOR, not the presence of a space. Ruby's grammar
// is range.rb's FULL_REGEX, /\A((?:[<>=])*)(.+)\Z/ -- a leading run of <, > or
// = -- and an intersection is two of those separated by a space. Keying on the
// space alone would also refuse `6.12.0-1.el10; 6.12.0-2.el10`, the
// multiversion string rpm.rb:105 joins with "; ", which type/package.rb:199
// matches by whole-value equality.
//
// Ruby's parser DOES read an operator-less value with a space as a range (two
// exact versions intersected), and best_version runs `dnf list --showduplicates`
// for it. Measured in the container for `8.1-1.el10 8.1-3.el10` and the joined
// form, nano absent and installed: no available version falls inside the
// intersection, so best_version returns the value unchanged, and Ruby runs the
// same dnf argv voxagent does, failing the same way. Only dnf's own output
// inside the failure detail differs. A value repeating one version, `X X`,
// would intersect to X and diverge; that is reasoned, not measured.
func isVersionRange(ensure string) bool {
	for _, token := range strings.Fields(ensure) {
		switch token[0] {
		case '<', '>', '=':
			return true
		}
	}
	return false
}

// SupportsValue implements ValueBounded.
//
// ensure must refuse an unimplemented value at pre-flight rather than at apply:
// package's carries an open-ended version string, so no finite set can express
// it, and an apply-time refusal would leave the rest of the catalog applied and
// the node half-converged.
//
// What remains refused is what this provider does not implement: `purged` and
// `disabled`, which Puppet implements and this provider declines; `held`,
// which Puppet does not implement either; and version RANGES, which Puppet
// resolves through best_version and this provider does not.
func (Package) SupportsValue(attr, value string) bool {
	switch attr {
	case "provider":
		// Ruby would accept yum here and resolve it to a different provider
		// class with its own commands. This one is dnf, and saying so is what
		// keeps a yum catalog from being applied by something it did not name.
		return value == "dnf"
	case "ensure":
		switch value {
		case "present", "installed", "absent", "latest":
			return true
		}
		// Every concrete version, and nothing else. `latest` is claimed by the
		// list above rather than by loosening this, so that ensureIsVersion
		// keeps meaning "names a concrete version" for both its callers --
		// Apply's default branch reads it that way too.
		return ensureIsVersion(value)
	default:
		// name, and any attribute a later task bounds. An attribute this
		// provider does not bound is answered by Attrs alone.
		return true
	}
}

// Refresh implements Provider. A package has nothing to refresh --
// reinstall_on_refresh is outside the bounded attribute set, so a package that
// is notified does not act on it, exactly as Ruby's default does not.
func (Package) Refresh(context.Context, catalog.Resource, bool) (bool, error) {
	return false, nil
}

// runner returns the seam, defaulting to the real one.
func (p Package) runner() commandRunner {
	if p.run == nil {
		return execRunner{}
	}
	return p.run
}

// Prefetch implements Provider. It reads the whole installed set in one rpm
// query, the way provider/package.rb's prefetch does, so that applying N
// packages costs one query rather than N.
//
// It refuses to run as anyone but root because provider/package/yum.rb:73
// raises unless Process.euid is 0 -- before any query, so a non-root run learns
// nothing about the node either. Measured: the refusal is type-wide, and the
// transaction wiring turns it into one err plus one "Skipping because provider prefetch failed"
// warning per package resource.
//
// dnf and rpm are resolved here rather than at each use because Ruby resolves
// both at provider load (dnf.rb:16) and treats a missing one as the provider
// being unusable at all -- not as a per-resource failure.
// It also resolves `ensure => latest` here, once for the whole run, for the
// reasons resolveUpdates sets out.
func (p Package) Prefetch(ctx context.Context, rs []catalog.Resource) (Provider, error) {
	// transaction.rb:379, before the provider's own prefetch runs. Only a
	// provider that defines prefetch is announced: posix (file, exec) and
	// systemd (service) define none, and 1-core/debug.txt has no such line.
	runlog.Debugf(ctx, "Puppet", "Prefetching %s resources for %s", *p.ProviderUsed(), p.Type())
	if os.Geteuid() != 0 {
		return nil, errors.New(notRootMessage)
	}
	dnfPath, err := exec.LookPath("dnf")
	if err != nil {
		return nil, fmt.Errorf("resolving the dnf provider's commands: %w", err)
	}
	rpmPath, err := exec.LookPath("rpm")
	if err != nil {
		return nil, fmt.Errorf("resolving the dnf provider's commands: %w", err)
	}
	// rpm.rb:70 runs this as `rpm -qa --nosignature --nodigest --qf ... | sort`.
	// Both flags suppress verification of a package FILE's signature and
	// digest, and neither changes what an rpm DATABASE query prints: measured
	// on this host, the two command lines produce byte-identical output for all
	// 349 installed packages. The sort is Ruby's grouping requirement
	// (rpm.rb:237-241 flushes on a name change) and a map needs no order, so
	// neither is reproduced -- and no shell pipeline is spawned to do it.
	//
	// A failed query is an error, never an empty set: an empty set reads as
	// "nothing is installed", which would install every package in the catalog.
	out, err := p.execute(ctx, rpmPath, "-qa", "--qf", rpmQueryFormat)
	if err != nil {
		return nil, err
	}
	p.dnfPath, p.rpmPath, p.state = dnfPath, rpmPath, parseInstalled(out)

	// After the installed set, and never before it: the guard on whether to run
	// check-update at all is "some `latest` resource is already installed".
	p.updates, p.checkUpdateFailure, err = p.resolveUpdates(ctx, rs)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// Apply implements Provider.
func (p Package) Apply(ctx context.Context, r catalog.Resource, noop bool) ([]Change, []Warning, error) {
	if p.state == nil {
		return nil, nil, errors.New("the dnf provider was not prefetched, so no installed-package state is known")
	}
	name := paramString(r, "name", r.Title)
	ensure := paramString(r, "ensure", "present")
	// A marked ensure is shown as [redacted] in the errors that name it (D-78:
	// Ruby leaks it).
	ensureMarked := isMarked(p, r, "ensure")
	current, installed := p.state[name]
	if !installed {
		// provider/package.rb's prefetch hands a provider only to resources
		// whose name an installed package carries. Any other keeps an empty
		// @property_hash, and `properties` fills it from `query || {:ensure =>
		// :purged}` -- so a VIRTUAL name such as `vi` reads as whatever
		// provides it. It happens at this resource's turn in the walk, not at
		// prefetch, so a provider an earlier resource installed is seen; and
		// under noop too, since it is a read. A failure is the resource's, not
		// ensure's: Ruby raises it from retrieve, before any property.
		var err error
		if current, installed, err = p.query(ctx, name); err != nil {
			return nil, nil, err
		}
	}

	// was is the ensure property's CURRENT value, which is the installed
	// version when there is one and "purged" when there is not (see
	// purgedValue). Every branch reports it and the message rule reads it, so
	// it is derived once here rather than per branch.
	was := purgedValue
	if installed {
		was = current
	}

	// The event name comes from the DECLARED ensure value alone, never from the
	// current state: type/package.rb:99 and :103 attach :package_installed to
	// the present newvalue and :package_removed to the absent one, and
	// property.rb reads the event off `should` before the sync runs. Measured
	// both ways round in the captured reports -- a package_changed event with
	// previous_value "purged", and a package_installed event that failed.
	switch ensure {
	case "present", "installed":
		if installed {
			return nil, nil, nil
		}
		// Is is "present" for `installed` too -- the type normalises the alias.
		ch := Change{Property: "ensure", Was: was, Is: "present", Name: "package_installed", Message: ensureMessage(was, "present")}
		if !noop {
			// installed is the alias the type normalises to present.
			logInstall(ctx, name, "present", ensureMarked, false, "", "install")
		}
		return p.sync(ctx, ch, noop, p.dnfPath, "-y", "install", name), nil, nil
	case "absent":
		if !installed {
			return nil, nil, nil
		}
		// Was is the installed VERSION, not the constant "present": ensure's
		// retrieve answers what is actually there, and a removal event's
		// previous_value is measured as "5.9-15.el10".
		//
		// rpm -e, not dnf remove. Neither dnf.rb nor yum.rb defines uninstall,
		// so the provider inherits rpm.rb:141-174, which for a resource with no
		// version, arch or install_only in its name resolves the identifier to
		// the bare name and runs `rpm -e`. dnf.rb:6-8 documents the consequence
		// -- removal does not take dependent packages with it -- which `dnf
		// remove` would not honour.
		ch := Change{Property: "ensure", Was: was, Is: "absent", Name: "package_removed", Message: ensureMessage(was, "absent")}
		return p.sync(ctx, ch, noop, p.rpmPath, "-e", name), nil, nil
	case "latest":
		// A package that is NOT installed short-circuits: type/package.rb:164
		// returns "out of sync" before provider.latest is ever called, so no
		// version is resolved, `latest` renders as itself in any message that
		// needs it, and this resource contributes nothing to the check-update
		// question. yum.rb:252-259 then picks `install` for it rather than
		// `upgrade` (AMENDMENT A15).
		if !installed {
			ch := Change{Property: "ensure", Was: was, Is: "latest", Name: "package_changed", Message: ensureMessage(was, "latest")}
			if !noop {
				logInstall(ctx, name, "latest", ensureMarked, false, "", "install")
			}
			return p.syncUpdate(ctx, ch, noop, "install", name, "", false), nil, nil
		}
		// The warning belongs to the resources that ASK for a latest version,
		// which is why it is raised here rather than for every `latest`
		// resource: in Ruby it is raised from inside provider.latest, which the
		// branch above never reaches.
		var warnings []Warning
		if p.checkUpdateFailure != "" {
			warnings = []Warning{{
				Source:  dnfProviderSource,
				Message: p.checkUpdateFailure,
				// AMENDMENT A13. Ruby logs this once per RUN because
				// latest_package_version memoizes check_updates on a class
				// variable, not because Puppet dedupes the message -- a plain
				// Puppet.warning does not dedupe at all. voxagent reaches the
				// same two observable counts by different means: Prefetch runs
				// check-update once, which matches the SUBPROCESS count, and
				// this key collapses the N resulting log lines into the one
				// the oracle emits.
				DedupeClass: dnfProviderSource,
			}}
		}
		// yum.rb's latest: an entry in the map is the newest available version,
		// and no entry means "pretend the current version is the latest", which
		// compares equal to itself.
		//
		// The comparison is EQUALITY against the resolved string
		// (type/package.rb:181-184), never rpmCompareEVR -- rpm_compare_evr's
		// own no-release early return carries the comment "This should NOT be
		// triggered if we're trying to ensure latest." Any pair rpm calls
		// equal but that differ as strings would otherwise be in sync forever,
		// and the package would never be upgraded again.
		newest, found := p.updates[name]
		if !found || newest == current {
			return nil, warnings, nil
		}
		// Is is the literal "latest" -- resource_harness.rb:188 stores
		// property.should -- while the message renders the resolved version,
		// which type/package.rb:218-224's should_to_s substitutes from the
		// @latest it cached during insync?. Measured, Package[which]:
		// desired_value "latest", message "ensure changed '2.21-42.el10' to
		// '0:2.21-44.el10'".
		ch := Change{
			Property: "ensure", Was: was, Is: "latest", IsDisplay: newest,
			Name: "package_changed", Message: ensureMessage(was, newest),
		}
		if !noop {
			logInstall(ctx, name, "latest", ensureMarked, true, current, "upgrade")
		}
		return p.syncUpdate(ctx, ch, noop, "upgrade", name, "", false), warnings, nil
	default:
		if !ensureIsVersion(ensure) {
			// Values this provider refuses. Pre-flight refuses all of them
			// through SupportsValue, so reaching here means a caller bypassed
			// it: fail the resource rather than report a change nothing made
			// -- or, worse, install a package named after the value.
			return nil, nil, fmt.Errorf("ensure => %s is not implemented", redactIf(ensureMarked, "%q", ensure))
		}
		if installed && versionInSync(current, ensure) {
			return nil, nil, nil
		}
		// package_changed, whether or not the package is installed: only the
		// present and absent newvalues declare an :event, so a version falls
		// back to property.rb:232-238's "#{resource.type}_changed". Measured
		// both ways -- Package[nano] upgraded and Package[lsof] installed from
		// purged are both package_changed (AMENDMENT A2).
		ch := Change{Property: "ensure", Was: was, Is: ensure, Name: "package_changed", Message: ensureMessage(was, ensure)}
		operation := versionOperation(ensure, current, installed)
		if !noop {
			logInstall(ctx, name, ensure, ensureMarked, installed, current, operation)
		}
		return p.syncUpdate(ctx, ch, noop, operation, name, ensure, ensureMarked), nil, nil
	}
}

// logInstall is the debug lines of yum.rb's install (:250-304) for the sync
// Apply is about to run, at the provider form. Ruby's should is the declared
// ensure, which a marked ensure shows as [redacted] (D-78: Ruby leaks it), in
// "Ensuring =>" and in the version the Upgrading and Downgrading lines name.
// operation is dnf's subcommand and current the installed version: for a
// declared version, Ruby's upgrade and downgrade lines follow versionOperation.
// The lines naming no value are Ruby's, whatever ensure is marked.
func logInstall(ctx context.Context, name, ensure string, marked, installed bool, current, operation string) {
	source := SourcesFrom(ctx).Provider
	should := redactIf(marked, "%s", ensure)
	runlog.Debugf(ctx, source, "Ensuring => %s", should)
	switch {
	case ensure == "latest" && !installed:
		runlog.Debugf(ctx, source, "Ensuring latest, but package is absent, so using install")
	case ensure == "latest":
		runlog.Debugf(ctx, source, "Ensuring latest, so using %s", operation)
	case installed && operation == "upgrade":
		runlog.Debugf(ctx, source, "Upgrading package %s from version %s to %s", name, current, should)
	case installed && operation == "downgrade":
		runlog.Debugf(ctx, source, "Downgrading package %s from version %s to %s", name, current, should)
	}
}

// ensureMessage is the ensure property's change_to_s, which is what the report
// carries as an event's message.
//
// Ruby reaches it in two hops: type/package.rb:226-235 returns "created" when
// the current value is nil, :absent or :purged, and otherwise defers to
// property/ensure.rb:54-61, which returns "removed" when the new value is
// :absent and "<name> changed <is> to <should>" otherwise.
//
// So the rule keys on the CURRENT value, not on the form of the declared one
// (AMENDMENT A10): `ensure => '4.98.0-7.el10'` on a purged package reports
// "created", the same as `ensure => present` does, and only a package that was
// already installed reports the transition.
func ensureMessage(was, is string) string {
	switch {
	case was == purgedValue:
		return "created"
	case is == "absent":
		return "removed"
	default:
		return fmt.Sprintf("ensure changed '%s' to '%s'", was, is)
	}
}

// versionInSync answers type/package.rb's question for a declared version,
// against the value stored for an INSTALLED package. A package that is not
// installed is never in sync -- rpm.rb:190's first line -- which is the
// `installed` guard at the call site.
//
// Two steps, in Ruby's order:
//
//  1. type/package.rb:199's `when *Array(is)` compares the declared string to
//     the whole stored value. Array("a; b") is ["a; b"], so this is plain
//     equality, and it is the only way a multiversion package is ever in sync.
//  2. On a miss, :203 falls through to the provider's insync? (rpm.rb:189-196).
//
// A package installed more than once is NEVER in sync at step 2, so it syncs on
// every run: rpm.rb:191 returns false for any value containing the multiversion
// separator unless install_only is set, and install_only is outside this
// provider's bounded attribute set, so it is always false here. That is
// reproduced DELIBERATELY. "The wanted version is among those installed, so do
// nothing" is a better behaviour and a divergence, and the standing directive is
// to match the original before improving on it.
func versionInSync(current, should string) bool {
	return current == should || providerInSync(current, should)
}

// providerInSync is step 2 alone: the provider's insync? (rpm.rb:189-196, and
// yum.rb:44's override, which defers to it for a concrete version). It is
// separate because verifyVersion asks it WITHOUT step 1 -- yum.rb:320 calls the
// provider's insync? directly -- so a joined value equal to the declaration is
// in sync before a sync and fails the check after one.
//
// rpm.rb:194 splits the value on the separator and compares each part. After
// :191 has rejected every value that contains the separator, that split can
// only ever yield the one value, so one comparison is the whole of it.
func providerInSync(current, should string) bool {
	if strings.Contains(current, multiVersionSeparator) {
		return false
	}
	return rpmCompareEVR(should, current) == 0
}

// versionOperation picks dnf's subcommand for a declared version, as
// yum.rb:290-302 does.
//
// Ruby starts from :install (:250) and overrides it only when the package is
// already installed and the versions differ, so a package that is not installed
// -- `current_package = query` is nil -- installs, and so does an installed one
// whose version compares EQUAL but was not in sync (a release-less declaration
// against a multiversion package reaches exactly that). Otherwise the wanted
// version being the lesser is :downgrade (:295) and the greater is
// update_command (:298), which dnf.rb:44-47 overrides to 'upgrade'.
//
// This is not cosmetic. `dnf -y install <older-version>` fails where
// `downgrade` succeeds, so a wrong operation turns a converging resource into a
// failed one -- and the word is quoted inside the failure message the report
// carries.
//
// The comparison is against the WHOLE stored value, joined separator and all,
// because that is what Ruby passes: yum.rb:295 reads current_package[:ensure],
// which rpm.rb:105 has already joined.
func versionOperation(should, current string, installed bool) string {
	if !installed {
		return "install"
	}
	switch rpmCompareEVR(should, current) {
	case -1:
		return "downgrade"
	case 1:
		return "upgrade"
	default:
		return "install"
	}
}

// sync runs one command and reports ch, carrying the failure on the change
// rather than returning it.
//
// A failed sync is ALWAYS attributed to ensure: resource_harness.rb:123's
// sync_if_needed wraps each property's sync and its rescue at :147 records a
// failure event for that property, which is what produces the measured
// "change from 'purged' to 'present' failed: ..." wording. A bare error return
// would name no property and discard the change entirely.
func (p Package) sync(ctx context.Context, ch Change, noop bool, path string, args ...string) []Change {
	if noop {
		// Nothing was attempted, so nothing can have failed. The transaction
		// reports ch as a would-be change.
		return []Change{ch}
	}
	if _, err := p.execute(ctx, path, args...); err != nil {
		ch.Err = err
	}
	return []Change{ch}
}

// syncUpdate syncs `latest` or an explicit version, reporting a failure the way
// type/package.rb's two upgrading newvalue blocks do.
//
// It differs from sync in exactly one way, and that difference is a reported
// value: newvalue(:latest) at :118 and the catch-all newvalue(/./) at :136 each
// wrap their provider call in a rescue and re-raise as
// "Could not update: <detail>", so the report carries that prefix ahead of the
// ExecutionFailure wording. newvalue(:present) at :99 and newvalue(:absent) at
// :103 do NOT -- which is why sync exists alongside this and the two are not
// merged. See couldNotUpdate.
//
// should is the declared version, or "" for `latest` -- yum.rb's install sets
// should = nil in its `when :latest` branch, exactly so. dnf is asked for the
// bare name when should is empty and name-should otherwise (yum.rb:283), and a
// sync that carried a should is then verified (see verifyVersion). operation is
// dnf's subcommand. The argv is quoted verbatim into a failure message, so both
// are output contracts (AMENDMENT A15).
//
// redact shows should as [redacted] in the sync failure (command line and
// output) and in that verification failure.
//
// The verification's failure takes the same "Could not update: " prefix: it is
// raised inside the same provider call newvalue(/./) rescues.
func (p Package) syncUpdate(ctx context.Context, ch Change, noop bool, operation, name, should string, redact bool) []Change {
	target := name
	if should != "" {
		target = name + "-" + should
	}
	syncCtx := ctx
	if redact && should != "" {
		syncCtx = withRedactedCommands(ctx)
	}
	changes := p.sync(syncCtx, ch, noop, p.dnfPath, "-y", operation, target)
	if redact && should != "" && changes[0].Err != nil {
		// The command line and dnf's output both name name-<should>.
		changes[0].Err = errors.New(strings.ReplaceAll(changes[0].Err.Error(), should, "[redacted]"))
	}
	if changes[0].Err == nil && !noop && should != "" {
		changes[0].Err = p.verifyVersion(ctx, name, should, redact)
	}
	if changes[0].Err != nil {
		changes[0].Err = fmt.Errorf("%s%w", couldNotUpdate, changes[0].Err)
	}
	return changes
}

// verifyVersion is yum.rb:313-321: after dnf succeeds at installing a declared
// version, query the package again and fail unless the provider's insync?
// accepts what is now installed. dnf exits 0 in two cases this catches: it
// resolved to some other version, or -- the case measured as D-14 -- the package
// is installonly (kernel-core), so dnf added the version BESIDE the old one and
// the re-query reads a joined value providerInSync always refuses. Ruby then
// reports a FAILED resource on a machine it changed, on every run.
//
// Nothing found is yum.rb:315's "Could not find package".
func (p Package) verifyVersion(ctx context.Context, name, should string, redact bool) error {
	version, found, err := p.query(ctx, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("Could not find package %s", name)
	}
	if !providerInSync(version, should) {
		return fmt.Errorf("Failed to update to version %s, got version %s instead", redactIf(redact, "%s", should), version)
	}
	return nil
}

// query is rpm.rb's `def query` for one name: `rpm -q NAME`, and on a non-zero
// exit -- with allow_virtual, which defaults to true and is outside the
// bounded attribute set -- a retry with --whatprovides appended. found is
// false only when both exit non-zero, which is Ruby's nil. A command that
// cannot run at all is returned as the seam's own error and not retried: what
// Ruby does then is unmeasured, and execute takes the same position. The
// --nosignature and --nodigest flags are dropped for the reason Prefetch
// gives.
//
// The version comes from the PROVIDING package when --whatprovides answered,
// so a virtual name reads as its provider's version.
func (p Package) query(ctx context.Context, name string) (version string, found bool, err error) {
	args := []string{"-q", name, "--qf", rpmQueryFormat}
	exit, out, err := p.runner().run(ctx, p.rpmPath, args...)
	if err == nil && exit != 0 {
		exit, out, err = p.runner().run(ctx, p.rpmPath, append(args, "--whatprovides")...)
		if err == nil && exit != 0 {
			return "", false, nil
		}
	}
	if err != nil {
		return "", false, err
	}
	// More than one package name happens when several packages provide a
	// virtual name: rpm.rb:249-253 then returns an Array, and query's
	// @property_hash.update(Array) raises this TypeError (the message measured
	// on the image's Ruby; the path itself is unmeasured, since no name in the
	// image has two installed providers). Zero cannot happen: rpm -q exits 0
	// only after printing a line for each match.
	installed := parseInstalled(out)
	if len(installed) != 1 {
		return "", false, errors.New("no implicit conversion of Array into Hash")
	}
	for _, v := range installed {
		version = v
	}
	return version, true, nil
}

// resolveUpdates answers "what is the newest available version of everything?"
// ONCE for the whole run, and reports the warning a failed answer raises.
//
// Ruby reaches once-per-run by memoization: yum.rb's latest_package_version
// stores check_updates' result in a class-level @latest_versions hash, so the
// first resource to ask pays for the query and the rest read the hash. Measured:
// three installed packages with `ensure => latest` against an unreachable repo
// emit ONE "Could not check for updates" warning, not three.
//
// voxagent reaches it from the other end, in Prefetch, because that is the only
// place that has both the whole resource list and the installed set before any
// Apply runs. The mechanism is not Ruby's; the two observable counts -- one
// warning and one `dnf check-update` per run -- are.
//
// The guard is not "some resource declares latest" but "some resource declares
// latest AND is already installed". type/package.rb:164's
// `return false if is == :absent || is == :purged` runs BEFORE provider.latest,
// so a purged resource never reaches check-update. Measured: a catalog whose
// `latest` resources are all purged runs it zero times and warns zero times.
func (p Package) resolveUpdates(ctx context.Context, rs []catalog.Resource) (map[string]string, string, error) {
	asked := false
	for _, r := range rs {
		if paramString(r, "ensure", "present") != "latest" {
			continue
		}
		if _, installed := p.state[paramString(r, "name", r.Title)]; installed {
			asked = true
			break
		}
	}
	if !asked {
		return nil, "", nil
	}
	// yum.rb:114's argv, verbatim: `-y`, no `-q`, and no package name -- it
	// asks for every available update at once (AMENDMENT A14). The `-y` lets
	// dnf write repo progress to stdout, which Ruby tolerates rather than
	// suppresses; parseCheckUpdate reproduces the filter that makes it
	// harmless.
	exit, out, err := p.runner().run(ctx, p.dnfPath, "-y", "check-update")
	if err != nil {
		// dnf could not be started at all, moments after exec.LookPath found
		// it. Ruby's behaviour here is UNMEASURED, so this fails the prefetch
		// rather than inventing a warning: the alternative -- swallowing it and
		// returning no updates -- would silently report every `latest` resource
		// as in sync, which is a false convergence claim.
		return nil, "", err
	}
	switch exit {
	case 100:
		// Updates are available and the output names them.
		return parseCheckUpdate(out), "", nil
	case 0:
		// None are available. yum.rb:127 debug-logs this and returns an empty
		// hash. The call is on the class, so the source is the class, as the
		// warning below has it, and the command is dnf's absolute path.
		runlog.Debugf(ctx, dnfProviderSource, "%s check-update exited with 0; no package updates available.", p.dnfPath)
		return nil, "", nil
	default:
		return nil, fmt.Sprintf(checkUpdateWarning, p.dnfPath, exit), nil
	}
}

// execute runs one command, returning its output and, on any failure, an error
// carrying Ruby's Puppet::ExecutionFailure wording.
//
// util/execution.rb:312 builds "Execution of '<command line>' returned
// <status>: <output.strip>", and that string is what the report carries
// verbatim -- measured in report-root.json as the tail of
// "change from 'purged' to 'present' failed: ". So the message is an OUTPUT
// CONTRACT, not a diagnostic: the command line, its flags, its absolute path
// and the stripped combined output are all part of a reported value.
func (p Package) execute(ctx context.Context, path string, args ...string) (string, error) {
	exit, out, err := p.runner().run(ctx, path, args...)
	if err != nil {
		// The command could not be run at all. Ruby raises a different
		// exception here, unmeasured, so this keeps the seam's own wording
		// rather than inventing Ruby's.
		return out, err
	}
	if exit != 0 {
		return out, fmt.Errorf("Execution of '%s' returned %d: %s",
			strings.Join(append([]string{path}, args...), " "), exit, strings.TrimSpace(out))
	}
	return out, nil
}
