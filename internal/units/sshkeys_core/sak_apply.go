package sshkeyscore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/nameservice"
	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/runlog"
)

// The parsed provider's run (spec §6, §6.3). pf: is Puppet's
// provider/parsedfile.rb; property.rb is Puppet's property.rb (openvox
// 8.34.0); suid: is util/suidmanager.rb.

// sakResource adapts a munged Ssh_authorized_key to parsedfile.Resource. Its
// name is the namevar, which Ruby's prefetch keys its resources by. target is
// should(:target) as resource computed it for this evaluation.
type sakResource struct {
	p         sakParams
	target    string
	hasTarget bool
}

// Name implements parsedfile.Resource.
func (r sakResource) Name() string { return r.p.Name }

// Title implements parsedfile.Resource.
func (r sakResource) Title() string { return r.p.Title }

// ShouldTarget implements parsedfile.Resource.
func (r sakResource) ShouldTarget() (string, bool) { return r.target, r.hasTarget }

// resource is p with should(:target) (kt:107-120): a given String target; none
// for `target => []`; else, with a user, File.expand_path of
// "~<user>/.ssh/authorized_keys", the home looked up now, so a User the run
// created earlier counts. "~" expands the name up to the first slash, and an
// empty name the agent's HOME. No such user, a failed lookup or a home that is
// not absolute is Ruby's rescued ArgumentError: debug "The required user is
// not yet present on the system" and no target.
func (s Sak) resource(ctx context.Context, p sakParams) sakResource {
	res := sakResource{p: p}
	if v, ok := p.Should["target"]; ok {
		res.target, res.hasTarget = v.S, v.Kind == parsedfile.Str
		return res
	}
	if !p.UserGiven {
		return res
	}
	name, rest, _ := strings.Cut(p.User+"/.ssh/authorized_keys", "/")
	home := os.Getenv("HOME")
	if name != "" {
		u, found, err := s.ns.User(name)
		if err != nil || !found {
			home = ""
		} else {
			home = u.Dir
		}
	}
	if !filepath.IsAbs(home) {
		runlog.Debugf(ctx, "Puppet", "The required user is not yet present on the system")
		return res
	}
	res.target, res.hasTarget = filepath.Clean(home+"/"+rest), true
	return res
}

// PrefetchAtTurn implements ral.TurnPrefetching: the default target is the
// home of a User the run may create first (spec §0).
func (Sak) PrefetchAtTurn() {}

// Prefetch implements ral.Provider (pf:224-255 through sakClass): every target
// is read as root through the flat filetype (spec §5.3) and each resource
// bound to its record. A read or parse error, a directory as target included,
// is the returned error, which the transaction reports type-wide.
func (s Sak) Prefetch(ctx context.Context, rs []catalog.Resource) (ral.Provider, error) {
	// transaction.rb:379.
	runlog.Debugf(ctx, "Puppet", "Prefetching %s resources for %s", parsedProvider, s.Type())
	resources := make([]parsedfile.Resource, 0, len(rs))
	for _, r := range rs {
		p, err := parseSak(r, s.release, s.encoding)
		if err != nil {
			return s, err
		}
		resources = append(resources, s.resource(ctx, p))
	}
	drop := &sakDrop{start: s.start, defaultContext: s.defaultContext}
	class := sakClass(func(target string) (parsedfile.FileType, error) {
		flat, err := parsedfile.NewFlat(target, 0, s.backup, s.relabel, s.encoding)
		if err != nil {
			return nil, err
		}
		return &sakFile{flat: flat, path: target, encoding: s.encoding, drop: drop}, nil
	}, s.now)
	st, err := ral.PrefetchParsedFile(ctx, class, resources)
	if err != nil {
		return s, err
	}
	s.state, s.drop = st, drop
	return s, nil
}

// sakEnsurable is the ssh_authorized_key type for ral.ParsedFileRun.Apply.
// user is not a record field: its getter returns the should, so it is always
// in sync and is not synced here (pf:181-202, spec §6.3).
var sakEnsurable = ral.ParsedFileEnsurable{
	Name:       "ssh_authorized_key",
	Properties: []string{"type", "key", "target", "options"},
	Current:    sakCurrent,
	Change:     sakChange,
	Store:      sakStore,
}

// Apply implements ral.Provider through ral.ParsedFileRun.Apply, with the
// provider's own flush. name is the key attribute the flush fills in
// (pf:448-462). The flush's own targets (D-102) are the ones the setters mark
// for this record (MarkModified): its target before any change, and the
// should. A Sensitive value is passed raw: the transaction redacts the marked
// properties.
func (s Sak) Apply(ctx context.Context, r catalog.Resource, noop bool) ([]ral.Change, []ral.Warning, error) {
	p, err := parseSak(r, s.release, s.encoding)
	if err != nil {
		return nil, nil, err
	}
	res := s.resource(ctx, p)
	should := make(map[string]parsedfile.Value, len(p.Should))
	for prop, v := range p.Should {
		if prop != "target" {
			should[prop] = v
		}
	}
	if res.hasTarget {
		should["target"] = parsedfile.Value{Kind: parsedfile.Str, S: res.target}
	}
	var own []string
	if rec := s.state.Records[p.Name]; rec.Target != "" || rec.OnDisk {
		own = append(own, rec.Target)
	}
	if res.hasTarget {
		own = append(own, res.target)
	}
	marked := func(attr string) bool { return ral.IsMarked(s, r, attr) }
	flush := func(ctx context.Context, run *parsedfile.Run, rec *parsedfile.Record) error {
		return s.flush(ctx, run, rec, res, own, marked)
	}
	keys := map[string]parsedfile.Value{"name": {Kind: parsedfile.Str, S: p.Name}}
	return s.state.Apply(ctx, &sakEnsurable, ral.ParsedFileShould{Resource: res, Ensure: p.Ensure, Should: should, Keys: keys, Flush: flush}, noop)
}

// flush is the provider's flush (kp:69-103), spec §6.3 items 1-5: the user and
// its uid are required, the target is backed up in the root parent, then the
// record's own targets are written as the user (drop_privileges true) or, when
// trusted_path passes, as the agent and chowned to the user.
func (s Sak) flush(ctx context.Context, run *parsedfile.Run, rec *parsedfile.Record, res sakResource, own []string, marked func(string) bool) error {
	p := res.p
	if !p.UserGiven {
		return errors.New("Cannot write SSH authorized keys without user")
	}
	userMarked := marked("user")
	user := rubyvalue.RedactIf(userMarked, "%s", p.User)
	uid, ok, err := nameservice.PosixUID(ctx, s.ns, p.User, userMarked)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("User '%s' does not exist", user)
	}
	// The target getter (pf:181-202): the record's, else the should.
	target := rec.Target
	if target == "" && !rec.OnDisk && res.hasTarget {
		target = res.target
	}
	if err := run.BackupTarget(ctx, target); err != nil {
		return err
	}
	if !p.DropPrivileges {
		return s.flushTrusted(ctx, run, rec, res, own, target, uid, userMarked)
	}

	cred, err := s.credential(p.User, marked)
	if err != nil {
		return err
	}
	s.drop.cred, s.drop.user = cred, user
	defer func() { s.drop.cred, s.drop.user = nil, "" }()
	if err := s.drop.mkdir(ctx, rubyDirname(target)); err != nil {
		return err
	}
	if err := run.FlushTargets(ctx, rec, res.target, res.hasTarget, own); err != nil {
		return err
	}
	return s.drop.chmod(ctx, target)
}

// credential is the identity the flush's ops run as (spec §3.4): none, so
// in-process, when the agent is not root (asuser yields, suid:65) or the
// user's uid is the agent's (change_user returns, suid:121; a name resolving
// to uid 0 included); else the user's, with its primary and supplementary
// groups (suid.Resolve). The helper thus never runs as uid 0.
func (s Sak) credential(user string, marked func(string) bool) (*syscall.Credential, error) {
	euid := s.euid()
	if euid != 0 {
		return nil, nil
	}
	cred, err := s.resolve(user, nil, marked)
	if err != nil {
		return nil, err
	}
	if cred.Uid == 0 {
		return nil, nil
	}
	return cred, nil
}

// flushTrusted is kp:92-102, drop_privileges false: unless trusted_path
// passes, "drop_privileges is false but the target path is not trusted";
// else the own targets are written in-process, then target is opened and
// fchowned to uid and the gid of the group named like the user
// (Puppet::Util.gid; -1, leaving the group, when there is none, spec §8 item
// 10) and fchmodded 0600. Those three fail with Ruby's SystemCallError text
// ([src]: rb_sysopen, rb_file_chown, rb_file_chmod).
func (s Sak) flushTrusted(ctx context.Context, run *parsedfile.Run, rec *parsedfile.Record, res sakResource, own []string,
	target string, uid int64, userMarked bool) error {
	// Ruby walks target only, then writes every modified target as root. A
	// move's old target is written too, so every own target must pass the walk
	// before anything is written (D-102): root never follows a link a user
	// placed in an untrusted directory.
	euid := s.euid()
	walked := map[string]bool{}
	for _, t := range append([]string{target}, own...) {
		if walked[t] {
			continue
		}
		walked[t] = true
		if !trustedPath(ctx, t, euid) {
			return errors.New("drop_privileges is false but the target path is not trusted")
		}
	}
	if err := run.FlushTargets(ctx, rec, res.target, res.hasTarget, own); err != nil {
		return err
	}
	gid, ok, err := nameservice.PosixGID(ctx, s.ns, res.p.User, userMarked)
	if err != nil {
		return err
	}
	if !ok {
		gid = -1
	}
	f, err := os.Open(target)
	if err != nil {
		return errors.New(rubyvalue.Errno(err, "rb_sysopen", target))
	}
	defer f.Close()
	// PosixUID and PosixGID bound ids to 0..4294967290, which int holds on
	// every 64-bit platform this provider runs on (spec §0: RHEL 10).
	if err := f.Chown(int(uid), int(gid)); err != nil {
		return errors.New(rubyvalue.Errno(err, "rb_file_chown", target))
	}
	if err := f.Chmod(0o600); err != nil {
		return errors.New(rubyvalue.Errno(err, "rb_file_chmod", target))
	}
	return nil
}

// sakStore puts a should value where sakCurrent reads it: target is the
// record's Target, an options list a private copy, the rest a field.
func sakStore(rec *parsedfile.Record, prop string, v parsedfile.Value) {
	switch prop {
	case "target":
		rec.Target = v.S
	case "options":
		if v.Kind == parsedfile.List {
			v.L = slices.Clone(v.L)
		}
		rec.Fields[prop] = v
	default:
		rec.Fields[prop] = v
	}
}

// sakCurrent is the property's retrieve (mk_resource_methods): target is the
// record's Target; options the field as is, where Absent is [:absent] (the
// post-parse default) and a missing field (Nil, a key_v1 line's) Ruby's
// scalar :absent; the rest the field or :absent.
func sakCurrent(rec *parsedfile.Record, prop string) parsedfile.Value {
	switch prop {
	case "target":
		return parsedfile.Value{Kind: parsedfile.Str, S: rec.Target}
	case "options":
		return rec.Fields[prop]
	}
	if v := rec.Fields[prop]; v.Kind != parsedfile.Nil {
		return v
	}
	return parsedfile.Value{Kind: parsedfile.Absent}
}

// sakChange is the change of prop from is to should, nil when in sync, with
// property.rb:205-219's messages: "defined '<prop>' as <should>" from a
// scalar :absent, else "<prop> changed <is> to <should>" (M8).
func sakChange(prop string, is, should parsedfile.Value) *ral.Change {
	if sakInSync(prop, is, should) {
		return nil
	}
	isShow, shouldShow := sakShow(prop, is), sakShow(prop, should)
	ch := &ral.Change{Property: prop, Was: sakReport(prop, is), Is: sakReport(prop, should),
		WasDisplay: isShow, IsDisplay: shouldShow, DisplayVerbatim: true}
	if is.Kind == parsedfile.Nil || is.Kind == parsedfile.Absent && prop != "options" {
		ch.Message = fmt.Sprintf("defined '%s' as %s", prop, shouldShow)
	} else {
		ch.Message = fmt.Sprintf("%s changed %s to %s", prop, isShow, shouldShow)
	}
	return ch
}

// sakInSync is the property's insync?. options is property.rb's
// array_matching :all rule: an empty should is in sync; else is must be an
// array equal to should, or to should stringified, so a parsed "absent"
// option matches the default [:absent] and [:absent] never matches a given
// "absent". target is is == should (kt:122-124); type and key String
// equality, :absent never in sync.
func sakInSync(prop string, is, should parsedfile.Value) bool {
	if prop != "options" {
		return is.Kind == parsedfile.Str && is.S == should.S
	}
	if should.Kind == parsedfile.List && len(should.L) == 0 {
		return true
	}
	switch is.Kind {
	case parsedfile.Absent:
		return should.Kind == parsedfile.Absent
	case parsedfile.List:
		return slices.Equal(is.L, sakOptions(should))
	}
	return false
}

// sakOptions is an options value's elements as strings: [:absent] is
// ["absent"].
func sakOptions(v parsedfile.Value) []string {
	if v.Kind == parsedfile.Absent {
		return []string{"absent"}
	}
	return v.L
}

// sakShow is a value as the change message shows it: an options value is a
// list, [:absent] included.
func sakShow(prop string, v parsedfile.Value) string {
	if prop == "options" && v.Kind == parsedfile.Absent {
		return "['absent']"
	}
	return v.Display()
}

// sakReport is a value as the change event carries it: an options value is a
// list, [:absent] included.
func sakReport(prop string, v parsedfile.Value) any {
	if prop == "options" && v.Kind == parsedfile.Absent {
		return []string{"absent"}
	}
	return v.ReportValue()
}
