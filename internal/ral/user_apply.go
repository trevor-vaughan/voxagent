package ral

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/command"
	"github.com/voxagent/voxagent/internal/lib/nameservice"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/runlog"
)

// Properties implements PropertyDeclaring: the properties useradd has the
// features for (spec §3.1). password is among them, so MarkSensitive can mark
// it: type/user.rb:280 declares it sensitive whatever the catalog says.
func (User) Properties() Attrs {
	return NewAttrs(append([]string{"ensure"}, userManagedProperties...)...)
}

// userCommands are the absolute paths of useradd's commands (useradd.rb:17),
// which confine the provider.
type userCommands struct{ add, del, mod, chage, chpasswd string }

// userCurrent is what retrieve reads for an existing user: passwd, the shadow
// entry when a shadow property is managed, and the groups when groups is.
type userCurrent struct {
	pw      nameservice.PasswdEntry
	sp      nameservice.ShadowEntry
	spFound bool
	groups  []string // getgrouplist order; never nil
}

// groupsWhitespace is useradd.rb:223's verify on the groups csv.
var groupsWhitespace = regexp.MustCompile(`\s`)

// Apply implements Provider (spec §5). ensure is synced first, and when it
// changes nothing else is (resource_harness.rb:84-99); otherwise each managed
// property out of sync, in type/user.rb's order. Under noop no command runs
// and no provider check (shell, gid resolution for the command) is made.
//
// Every create command is redacted when any property is marked Sensitive, as
// password always is (has_sensitive_data?, useradd.rb:311-318, asked by
// nameservice.rb:162); on modify only a marked property's own command is (set
// asks has_sensitive_data?(param), nameservice.rb:270), chpasswd always is,
// and delete never is (M7 debug.txt).
func (u User) Apply(ctx context.Context, r catalog.Resource, noop bool) ([]Change, []Warning, error) {
	p, err := parseUserParams(r)
	if err != nil {
		return nil, nil, err
	}
	marked, _ := MarkSensitive(u, r)
	var cmds userCommands
	for _, c := range []struct {
		name string
		dst  *string
	}{{"useradd", &cmds.add}, {"userdel", &cmds.del}, {"usermod", &cmds.mod}, {"chage", &cmds.chage}, {"chpasswd", &cmds.chpasswd}} {
		if *c.dst, err = nameservice.ShadowCommand(c.name); err != nil {
			// transaction.rb:130's words for a provider its commands confine.
			return nil, nil, errors.New("Provider useradd is not functional on this host")
		}
	}
	ns := u.nss()
	pw, exists, err := ns.User(p.Name)
	if err != nil {
		return nil, nil, err
	}
	suffix := ""
	if noop {
		suffix = " (noop)"
	}

	switch {
	case p.Ensure == "absent" && exists:
		ch := Change{Property: "ensure", Was: "present", Is: "absent", Message: "removed" + suffix, Name: "user_removed"}
		if !noop {
			argv := []string{cmds.del}
			if p.ManageHome {
				argv = append(argv, "-r")
			}
			if err := u.exec(ctx, command.ArgvOpts{}, append(argv, p.Name)); err != nil {
				ch.Err = fmt.Errorf("Could not delete user %s: %w", p.Name, err)
			}
		}
		return []Change{ch}, nil, nil
	case p.Ensure == "present" && !exists:
		ch := Change{Property: "ensure", Was: "absent", Is: "present", Message: "created" + suffix, Name: "user_created"}
		if !noop {
			ch.Err = u.create(ctx, r, p, cmds, len(marked) > 0)
		}
		return []Change{ch}, nil, nil
	case !exists:
		runlog.Debugf(ctx, sources.SourcesFrom(ctx).Resource, "Nothing to manage: no ensure and the resource doesn't exist")
		return nil, nil, nil
	}

	cur := userCurrent{pw: pw}
	if p.Password != nil || p.MinAge != nil || p.MaxAge != nil || p.WarnDays != nil || p.Expiry != nil {
		if cur.sp, cur.spFound, err = ns.Shadow(p.Name); err != nil {
			return nil, nil, err
		}
	}
	if p.Groups != nil {
		groups, err := nameservice.GroupsOf(ns, p.Name)
		if err != nil {
			return nil, nil, err
		}
		cur.groups = append([]string{}, groups...)
	}
	return u.modify(ctx, p, cur, cmds, marked, noop), nil, nil
}

// create is useradd.rb:398-408 around nameservice.rb:154-170: the shell check,
// useradd, chage when an age is set, then usermod -p when password is. redact
// is has_sensitive_data?: whether any property is marked.
// Puppet::Errors (the shell check, the wrapped command failures) fail ensure
// as they are; any other error is wrapped by property.rb:492.
func (u User) create(ctx context.Context, r catalog.Resource, p userParams, cmds userCommands, redact bool) error {
	ensureErr := func(err error) error {
		return fmt.Errorf("Could not set 'present' on ensure: %s%s", err, rubyvalue.ErrorLocation(r.File, r.Line))
	}
	if p.Shell != nil {
		if err := checkShell(*p.Shell); err != nil {
			return err
		}
	}
	ns := u.nss()
	argv := []string{cmds.add}
	if p.GID == nil {
		_, ok, err := nameservice.PosixGID(ctx, ns, p.Name)
		if err != nil {
			return ensureErr(err)
		}
		if ok {
			argv = append(argv, "-g", p.Name)
		}
	}
	// add_properties (useradd.rb:284-297): alphabetical, skipping a nil or
	// empty should.
	flag := func(f, v string) {
		if v != "" {
			argv = append(argv, f, v)
		}
	}
	if p.Comment != nil {
		flag("-c", *p.Comment)
	}
	if p.Expiry != nil {
		// :absent is not empty; its munge makes it the empty argument.
		argv = append(argv, "-e", expiryArg(*p.Expiry))
	}
	if p.GID != nil {
		flag("-g", idArg(p.GID))
	}
	if p.Groups != nil {
		var current []string
		if !p.Inclusive {
			// The should csv reads the user's groups; for a user that does not
			// exist that is the fallback scan (posix.rb:38-70).
			var err error
			if current, err = nameservice.GroupsOf(ns, p.Name); err != nil {
				return ensureErr(err)
			}
		}
		flag("-G", groupsShould(p.Groups, current, p.Inclusive))
	}
	if p.Home != nil {
		flag("-d", *p.Home)
	}
	if p.Shell != nil {
		flag("-s", *p.Shell)
	}
	if p.UID != nil {
		flag("-u", idArg(p.UID))
	}
	if p.AllowDupe {
		argv = append(argv, "-o")
	}
	if p.ManageHome {
		argv = append(argv, "-m")
	} else {
		argv = append(argv, "-M")
	}
	if p.System {
		argv = append(argv, "-r")
	}
	opts := command.ArgvOpts{Redact: redact}
	if err := u.exec(ctx, opts, append(argv, p.Name)); err != nil {
		return fmt.Errorf("Could not create user %s: %w", p.Name, err)
	}
	// passcmd (useradd.rb:370-383).
	chage := []string{cmds.chage}
	for _, a := range []struct {
		flag string
		v    *int64
	}{{"-m", p.MinAge}, {"-M", p.MaxAge}, {"-W", p.WarnDays}} {
		if a.v != nil {
			chage = append(chage, a.flag, strconv.FormatInt(*a.v, 10))
		}
	}
	if len(chage) > 1 {
		if err := u.exec(ctx, opts, append(chage, p.Name)); err != nil {
			return fmt.Errorf("Could not create user %s: %w", p.Name, err)
		}
	}
	if p.Password != nil {
		// set(:password) asks has_sensitive_data?(:password): always marked.
		if err := u.exec(ctx, command.ArgvOpts{Redact: true}, []string{cmds.mod, "-p", *p.Password, p.Name}); err != nil {
			return fmt.Errorf("Could not set password on user[%s]: %w", p.Name, err)
		}
	}
	return nil
}

// modify syncs each managed property that is out of sync, in type/user.rb's
// order. Messages follow property.rb:205-211 (defined/undefined/changed), with
// values shown as format_value_for_display shows them; the failure line's
// displays are the same values, given verbatim. A marked property's command
// is redacted.
func (u User) modify(ctx context.Context, p userParams, cur userCurrent, cmds userCommands, marked Attrs, noop bool) []Change {
	suffix := ""
	if noop {
		suffix = " (noop)"
	}
	var changes []Change
	// add records one change. was/is are the report's values ("absent" for
	// :absent); wasShow/isShow their displays, which the failure line uses
	// verbatim; shown is the should as the message shows it; sync runs it live.
	add := func(prop string, was, is any, wasShow, isShow, shown string, wasAbsent, isAbsent bool, sync func() error) {
		ch := Change{Property: prop, Was: was, Is: is, WasDisplay: wasShow, IsDisplay: isShow, DisplayVerbatim: true}
		switch {
		case wasAbsent:
			ch.Message = fmt.Sprintf("defined '%s' as %s", prop, shown)
		case isAbsent:
			ch.Message = fmt.Sprintf("undefined '%s' from %s", prop, wasShow)
		default:
			ch.Message = fmt.Sprintf("%s changed %s to %s", prop, wasShow, shown)
		}
		ch.Message += suffix
		if !noop {
			ch.Err = sync()
		}
		changes = append(changes, ch)
	}
	// set is nameservice.rb:265-276 for one property: modifycmd, run redacted
	// when the property is marked, and a command failure wrapped with the
	// property's name.
	set := func(prop, cmd, flag, value string, extra ...string) error {
		argv := append([]string{cmd, flag, value}, extra...)
		_, redact := marked[prop]
		if err := u.exec(ctx, command.ArgvOpts{Redact: redact}, append(argv, p.Name)); err != nil {
			return fmt.Errorf("Could not set %s on user[%s]: %w", prop, p.Name, err)
		}
		return nil
	}
	text := func(prop, flag string, current string, should *string, check func() error) {
		if should == nil || current == *should {
			return
		}
		add(prop, current, *should, rubyvalue.PuppetQuote(current), rubyvalue.PuppetQuote(*should), rubyvalue.PuppetQuote(*should), false, false, func() error {
			if check != nil {
				if err := check(); err != nil {
					return err
				}
			}
			return set(prop, cmds.mod, flag, *should)
		})
	}

	text("home", "-d", cur.pw.Dir, p.Home, nil)
	if p.UID != nil && p.UID != cur.pw.UID {
		var extra []string
		if p.AllowDupe {
			extra = []string{"-o"}
		}
		add("uid", cur.pw.UID, p.UID, puppetValue(cur.pw.UID), puppetValue(p.UID), puppetValue(p.UID), false, false, func() error {
			return set("uid", cmds.mod, "-u", idArg(p.UID), extra...)
		})
	}
	if p.GID != nil {
		gid, ok, err := nameservice.PosixGID(ctx, u.nss(), p.GID)
		if err != nil || !ok || gid != cur.pw.GID {
			add("gid", cur.pw.GID, p.GID, puppetValue(cur.pw.GID), puppetValue(p.GID), puppetValue(p.GID), false, false, func() error {
				if err != nil {
					return err
				}
				// type/user.rb:172-186 resolves the value again to sync it.
				gid, ok, err := nameservice.PosixGID(ctx, u.nss(), p.GID)
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("Could not find group(s) %s", idArg(p.GID))
				}
				return set("gid", cmds.mod, "-g", strconv.FormatInt(gid, 10))
			})
			if err != nil {
				// The in-sync test itself failed (insync?, type/user.rb:162-170):
				// a failure under noop too (resource_harness.rb:147).
				changes[len(changes)-1].Err = err
			}
		}
	}
	text("comment", "-c", cur.pw.Gecos, p.Comment, nil)
	text("shell", "-s", cur.pw.Shell, p.Shell, func() error { return checkShell(*p.Shell) })

	if p.Password != nil && !(cur.spFound && cur.sp.Hash == *p.Password) {
		// A sensitive property's values and messages are always redacted
		// (resource_harness.rb:182-185, :238-255), so the hash never leaves
		// this function except on chpasswd's stdin.
		ch := Change{Property: "password", Was: "[redacted]", Is: "[redacted]",
			WasDisplay: "[redacted]", IsDisplay: "[redacted]", DisplayVerbatim: true,
			Message: "changed [redacted] to [redacted]"}
		if noop {
			ch.Message = "current_value [redacted], should be [redacted] (noop)"
		} else {
			ch.Err = u.chpasswd(ctx, cmds.chpasswd, p.Name, *p.Password)
		}
		changes = append(changes, ch)
	}

	for _, a := range []struct {
		prop, flag string
		current    int64
		should     *int64
	}{
		{"password_min_age", "-m", cur.sp.Min, p.MinAge},
		{"password_max_age", "-M", cur.sp.Max, p.MaxAge},
		{"password_warn_days", "-W", cur.sp.Warn, p.WarnDays},
	} {
		if a.should == nil || cur.spFound && a.current == *a.should {
			continue
		}
		var was any = "absent"
		if cur.spFound {
			was = a.current
		}
		add(a.prop, was, *a.should, puppetValue(was), puppetValue(*a.should), puppetValue(*a.should), !cur.spFound, false, func() error {
			return set(a.prop, cmds.chage, a.flag, strconv.FormatInt(*a.should, 10))
		})
	}

	if p.Groups != nil {
		should := groupsShould(p.Groups, cur.groups, p.Inclusive)
		if strings.Join(slices.Sorted(slices.Values(cur.groups)), ",") != should {
			// change_to_s (type/user.rb:353-364) shows the csv split into a list.
			shown := make([]string, 0)
			for _, g := range rubyvalue.SplitComma(should) {
				shown = append(shown, rubyvalue.PuppetQuote(g))
			}
			add("groups", cur.groups, should, strings.Join(cur.groups, ","), rubyvalue.PuppetQuote(should), "["+strings.Join(shown, ", ")+"]", false, false, func() error {
				if groupsWhitespace.MatchString(should) {
					return fmt.Errorf("Invalid value %s: Groups must be comma-separated", should)
				}
				return set("groups", cmds.mod, "-G", should)
			})
		}
	}

	if p.Expiry != nil {
		current := "absent"
		if cur.spFound && cur.sp.Expire != -1 {
			current = time.Unix(cur.sp.Expire*86400, 0).UTC().Format("2006-01-02")
		}
		if current != *p.Expiry {
			add("expiry", current, *p.Expiry, rubyvalue.PuppetQuote(current), rubyvalue.PuppetQuote(*p.Expiry), rubyvalue.PuppetQuote(*p.Expiry), current == "absent", *p.Expiry == "absent", func() error {
				return set("expiry", cmds.mod, "-e", expiryArg(*p.Expiry))
			})
		}
	}
	return changes
}

// chpasswd is useradd.rb:188-217: "<name>:<hash>\n" on chpasswd -e's stdin
// (a pipe, where Ruby uses a Tempfile), the command redacted. The exit status
// is ignored; any output is the failure, unwrapped and verbatim.
func (u User) chpasswd(ctx context.Context, cmd, name, hash string) error {
	_, out, err := u.shadowRunner().Run(ctx, command.ArgvOpts{Stdin: []byte(name + ":" + hash + "\n"), Redact: true}, cmd, "-e")
	if err != nil {
		return fmt.Errorf("Could not set password on user[%s]: %w", name, err)
	}
	if out != "" {
		return errors.New("chpasswd said " + out)
	}
	return nil
}

// exec runs one command with Puppet's failonfail: a non-zero exit is a
// Puppet::ExecutionFailure naming the command, or [redacted] under o.Redact.
func (u User) exec(ctx context.Context, o command.ArgvOpts, argv []string) error {
	code, out, err := u.shadowRunner().Run(ctx, o, argv[0], argv[1:]...)
	if err != nil {
		return err
	}
	if code != 0 {
		return command.ExecFailure(argv, o.Redact, code, out)
	}
	return nil
}

func (u User) shadowRunner() command.ShadowRunner {
	if u.runner == nil {
		return command.HostShadowRunner{}
	}
	return u.runner
}

func (u User) nss() nameservice.NameService {
	if u.ns == nil {
		return nameservice.Host
	}
	return u.ns
}

// checkShell is check_valid_shell (useradd.rb:248-255): File.exist? then
// File.executable?.
func checkShell(shell string) error {
	fi, err := os.Stat(shell)
	if err != nil {
		return fmt.Errorf("Shell %s must exist", shell)
	}
	if !command.Executable(shell, fi) {
		return fmt.Errorf("Shell %s must be executable", shell)
	}
	return nil
}

// groupsShould is Puppet::Property::List#should (property/list.rb:33-44): the
// wanted groups, plus the current ones unless inclusive (then uniq'd), sorted
// and joined by commas.
func groupsShould(should, current []string, inclusive bool) string {
	if inclusive {
		return strings.Join(slices.Sorted(slices.Values(should)), ",")
	}
	return strings.Join(rubyvalue.UniqSorted(append(slices.Clone(should), current...)), ",")
}

// expiryArg is useradd.rb:26-44's munge on EL: :absent is the empty argument.
func expiryArg(v string) string {
	if v == "absent" {
		return ""
	}
	return v
}

// idArg renders a munged uid or gid should (an int64, or a string kept as
// written) as a command argument and as Ruby's to_s.
func idArg(v any) string {
	if n, ok := v.(int64); ok {
		return strconv.FormatInt(n, 10)
	}
	return v.(string)
}

// puppetValue is format_value_for_display for the user's scalar values:
// Integers bare, strings (and :absent, as "absent") puppet-quoted.
func puppetValue(v any) string {
	if n, ok := v.(int64); ok {
		return strconv.FormatInt(n, 10)
	}
	return rubyvalue.PuppetQuote(v.(string))
}
