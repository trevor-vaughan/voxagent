package ral

import "strconv"

// The value helpers augeasproviders_ssh 7.0.0's ssh_config (SCP) and
// sshd_config (SDP) providers share, spec §6: get_value, set_simple_value and
// set_array_value, each run inside an augopen block on its handle. sshd
// selects SDP's variant where the two differ.

// getValue is get_value (SCP:113-124, SDP:33-44): each node path matches,
// read whole, or element by element when it has a numbered child, flattened.
// It is a Ruby Array: each element a string, or nil for a node without a
// value (a RemoteForward, for one).
func getValue(a *rubyAug, path string) ([]any, error) {
	nodes, err := a.match(path)
	if err != nil {
		return nil, err
	}
	var out []any
	read := func(p string) {
		if v, ok := a.get(p); ok {
			out = append(out, v)
		} else {
			out = append(out, nil)
		}
	}
	for _, vp := range nodes {
		numbered, err := a.match(vp + "/*[label()=~regexp('[0-9]*')]")
		if err != nil {
			return nil, err
		}
		if len(numbered) == 0 {
			read(vp)
			continue
		}
		subs, err := a.match(vp + "/*")
		if err != nil {
			return nil, err
		}
		for _, svp := range subs {
			read(svp)
		}
	}
	return out, nil
}

// setSimpleValue is set_simple_value (SCP:145-175, SDP:74-113): the entries
// path matches take values in order and surplus entries are removed; each
// value left is a new label entry after the last one placed, else after a
// commented-out line for label (commentFlags is the regexp's flags argument,
// "" or ", 'i'"), else for sshd before the first Match or after the last
// node. ssh_config then rebinds $resource to path. values are as getValue's:
// a nil consumes an entry by removing it, and is set valueless beyond them.
func setSimpleValue(a *rubyAug, base, path, label string, values []any, commentFlags string, sshd bool) error {
	entries, err := a.match(path)
	if err != nil {
		return err
	}
	rest := values
	lastsp := ""
	for _, sp := range entries {
		// Ruby's value.shift: nil both when values ran out and for a nil.
		var v any
		if len(rest) > 0 {
			v, rest = rest[0], rest[1:]
		}
		if v == nil {
			a.rm(sp)
			continue
		}
		a.set(sp, v.(string))
		lastsp = sp
	}
	for _, v := range rest {
		if lastsp != "" {
			a.insert(lastsp, label, false)
		} else {
			commented, err := a.match(base + "/#comment[.=~regexp('" + label + "([^a-z.].*)?'" + commentFlags + ")]")
			if err != nil {
				return err
			}
			switch {
			case len(commented) > 0:
				a.insert(commented[0], label, false)
			case sshd:
				if err := insertBeforeMatch(a, base, label); err != nil {
					return err
				}
			}
		}
		setOrClear(a, path+"[last()]", v)
		last, err := a.match(path + "[last()]")
		if err != nil {
			return err
		}
		// Ruby's `match(...)[0]`: nil when nothing matched.
		lastsp = ""
		if len(last) > 0 {
			lastsp = last[0]
		}
	}
	if !sshd {
		a.defvar("resource", path)
	}
	return nil
}

// setArrayValue is set_array_value (SCP:134-143, SDP:54-72): for sshd a new
// label entry first (before the first Match, else after the last node); then
// path's children and every entry but the first are removed, and the values
// set as children 1, 2, … (a nil valueless).
func setArrayValue(a *rubyAug, base, path, label string, values []any, sshd bool) error {
	if sshd {
		if err := insertBeforeMatch(a, base, label); err != nil {
			return err
		}
	}
	a.rm(path + "/*")
	a.rm(path + "[position() != 1]")
	for i, v := range values {
		setOrClear(a, path+"/"+strconv.Itoa(i+1), v)
	}
	return nil
}

// setOrClear is Augeas#set with a String or nil value.
func setOrClear(a *rubyAug, path string, v any) {
	if v == nil {
		a.clear(path)
	} else {
		a.set(path, v.(string))
	}
}

// insertBeforeMatch is SDP's placement of a new entry in the main section:
// before the first Match block, or after the last node when there is none.
func insertBeforeMatch(a *rubyAug, base, label string) error {
	matches, err := a.match(base + "/Match")
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		a.insert(base+"/*", label, false)
	} else {
		a.insert(base+"/Match[1]", label, true)
	}
	return nil
}
