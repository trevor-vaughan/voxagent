package file

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
)

// flatLogs is a context whose logger keeps every entry, and what it kept.
func flatLogs(t *testing.T) (context.Context, *[]report.LogEntry) {
	t.Helper()
	var logs []report.LogEntry
	l := runlog.New("debug", func(e report.LogEntry) { logs = append(logs, e) }, nil)
	return runlog.WithLogger(context.Background(), l), &logs
}

// withSELinux makes se the host's SELinux for the test.
func withSELinux(t *testing.T, se selinuxOps) {
	t.Helper()
	old := defaultSELinux
	defaultSELinux = se
	t.Cleanup(func() { defaultSELinux = old })
}

func TestBucketBackupEntry(t *testing.T) {
	dir, vardir := t.TempDir(), t.TempDir()
	target := filepath.Join(dir, "hosts")
	mustWrite(t, target, "old\n")
	ctx, _ := flatLogs(t)
	if err := BucketBackup(vardir)(ctx, target); err != nil {
		t.Fatal(err)
	}
	entry := shardDir(vardir, sha("old\n"))
	if got := readString(t, filepath.Join(entry, "contents")); got != "old\n" {
		t.Errorf("contents = %q", got)
	}
	if got, want := readString(t, filepath.Join(entry, "paths")), realPath(t, target)+"\n"; got != want {
		t.Errorf("paths = %q, want %q", got, want)
	}
}

func TestBucketBackupRecordsTheResolvedPathOfALink(t *testing.T) {
	dir, vardir := t.TempDir(), t.TempDir()
	real, link := filepath.Join(dir, "ak_target"), filepath.Join(dir, "authorized_keys")
	mustWrite(t, real, "key\n")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	ctx, logs := flatLogs(t)
	if err := BucketBackup(vardir)(ctx, link); err != nil {
		t.Fatal(err)
	}
	if got, want := readString(t, filepath.Join(shardDir(vardir, sha("key\n")), "paths")), realPath(t, real)+"\n"; got != want {
		t.Errorf("paths = %q, want the resolved %q", got, want)
	}
	if (*logs)[0].Message != "Computing checksum on file "+link {
		t.Errorf("first log = %q, want the path as given", (*logs)[0].Message)
	}
}

func TestBucketBackupLogs(t *testing.T) {
	dir, vardir := t.TempDir(), t.TempDir()
	target := filepath.Join(dir, "hosts")
	mustWrite(t, target, "old\n")
	ctx, logs := flatLogs(t)
	if err := BucketBackup(vardir)(ctx, target); err != nil {
		t.Fatal(err)
	}
	paths := filepath.Join(shardDir(vardir, sha("old\n")), "paths")
	want := []report.LogEntry{
		{Level: "info", Source: "Puppet", Message: "Computing checksum on file " + target},
		{Level: "debug", Source: "Puppet", Message: "Locked '" + paths + "'"},
		{Level: "debug", Source: "Puppet", Message: "Unlocked '" + paths + "'"},
	}
	if len(*logs) != len(want) {
		t.Fatalf("logs = %+v, want %+v", *logs, want)
	}
	for i, w := range want {
		g := (*logs)[i]
		if g.Level != w.Level || g.Source != w.Source || g.Message != w.Message {
			t.Errorf("log %d = %+v, want %+v", i, g, w)
		}
	}
	if !regexp.MustCompile(`/clientbucket/[0-9a-f]/[0-9a-f]/[0-9a-f]/[0-9a-f]/[0-9a-f]/[0-9a-f]/[0-9a-f]/[0-9a-f]/[0-9a-f]{64}/paths'$`).MatchString((*logs)[1].Message) {
		t.Errorf("Locked line %q is not <8 one-hex dirs>/<sha256>/paths", (*logs)[1].Message)
	}
}

func TestBucketBackupMissingTargetIsSilent(t *testing.T) {
	dir, vardir := t.TempDir(), t.TempDir()
	ctx, logs := flatLogs(t)
	for _, p := range []string{filepath.Join(dir, "missing"), danglingLink(t, dir)} {
		if err := BucketBackup(vardir)(ctx, p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if len(*logs) != 0 {
		t.Errorf("logs = %+v, want none", *logs)
	}
	if _, err := os.Lstat(filepath.Join(vardir, "clientbucket")); !os.IsNotExist(err) {
		t.Errorf("a bucket was made for a missing target (%v)", err)
	}
}

func danglingLink(t *testing.T, dir string) string {
	t.Helper()
	link := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), link); err != nil {
		t.Fatal(err)
	}
	return link
}

func TestBucketBackupFailureIsAnError(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "hosts")
	mustWrite(t, target, "old\n")
	ctx, _ := flatLogs(t)
	if err := BucketBackup("")(ctx, target); err == nil {
		t.Error("an unset vardir backed up without error")
	}
}

// A directory is not backed up file by file, as a file resource's is: Ruby's
// dipper reads the path as one file and fails (measured in the oracle image,
// puppet/file_bucket/dipper.rb backup), after its checksum line.
func TestBucketBackupOfADirectoryIsRubysEISDIR(t *testing.T) {
	dir, vardir := t.TempDir(), t.TempDir()
	target := filepath.Join(dir, "authorized_keys")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(target, "secret"), "secret\n")
	ctx, logs := flatLogs(t)
	err := BucketBackup(vardir)(ctx, target)
	want := "Could not back up " + target + ": Is a directory @ io_fread - " + target
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
	if len(*logs) != 1 || (*logs)[0].Message != "Computing checksum on file "+target {
		t.Errorf("logs = %+v, want only the checksum line", *logs)
	}
	if _, err := os.Lstat(filepath.Join(vardir, "clientbucket")); !os.IsNotExist(err) {
		t.Errorf("a directory's files were bucketed (%v)", err)
	}
}

// A regular target swapped for a directory after BucketBackup's checks is
// refused the same way: nothing below the directory reaches the bucket.
func TestBucketBackupNeverDescendsIntoASwappedInDirectory(t *testing.T) {
	dir, vardir := t.TempDir(), t.TempDir()
	target := filepath.Join(dir, "authorized_keys")
	mustWrite(t, target, "key\n")
	resolved, descended := realPath(t, target), false
	backupSwapWindow = func(at, path string) {
		switch {
		case at == "lstat" && path == resolved:
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			mustWrite(t, filepath.Join(path, "secret"), "secret\n")
		case at == "open":
			descended = true
		}
	}
	t.Cleanup(func() { backupSwapWindow = nil })
	ctx, _ := flatLogs(t)
	err := BucketBackup(vardir)(ctx, target)
	want := "Could not back up " + target + ": Is a directory @ io_fread - " + target
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
	if descended {
		t.Error("the backup opened the swapped-in directory")
	}
	if _, err := os.Lstat(shardDir(vardir, sha("secret\n"))); !os.IsNotExist(err) {
		t.Errorf("a file below the swapped-in directory was bucketed (%v)", err)
	}
}

func TestBucketBackupTwiceKeepsOneRecord(t *testing.T) {
	dir, vardir := t.TempDir(), t.TempDir()
	target := filepath.Join(dir, "hosts")
	mustWrite(t, target, "old\n")
	ctx, logs := flatLogs(t)
	for range 2 {
		if err := BucketBackup(vardir)(ctx, target); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := readString(t, filepath.Join(shardDir(vardir, sha("old\n")), "paths")), realPath(t, target)+"\n"; got != want {
		t.Errorf("paths = %q, want one line", got)
	}
	if len(*logs) != 4 {
		t.Errorf("logs = %+v, want the second backup to log only its checksum", *logs)
	}
}

func TestRelabelHookWarnsAndSucceeds(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	mustWrite(t, p, "x")
	se := enabledSE(p, tmpT)
	se.setErr = errors.New("denied")
	withSELinux(t, se)
	ctx, logs := flatLogs(t)
	if err := RelabelHook(ctx, p); err != nil {
		t.Fatalf("RelabelHook = %v, want nil", err)
	}
	want := "Failed to set SELinux context " + etcT.String() + " on " + p
	if len(*logs) != 1 || (*logs)[0].Level != "warning" || (*logs)[0].Source != "Puppet" || (*logs)[0].Message != want {
		t.Errorf("logs = %+v, want one Puppet warning %q", *logs, want)
	}
}

func TestRelabelHookSuccessIsSilent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	mustWrite(t, p, "x")
	se := enabledSE(p, tmpT)
	withSELinux(t, se)
	ctx, logs := flatLogs(t)
	if err := RelabelHook(ctx, p); err != nil {
		t.Fatal(err)
	}
	if len(*logs) != 0 || len(se.sets) != 1 || se.sets[0] != p+"="+etcT.String() {
		t.Errorf("logs = %+v, sets = %v", *logs, se.sets)
	}
}

func TestRelabelSetsTheDefaultOnlyWhenItDiffers(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	mustWrite(t, p, "x")
	se := enabledSE(p, etcT)
	withSELinux(t, se)
	got, err := Relabel(context.Background(), p)
	if got != "" || err != nil || len(se.sets) != 0 {
		t.Errorf("Relabel with the default already set = %q, %v, sets %v", got, err, se.sets)
	}
	se.cur[p] = tmpT.String()
	got, err = Relabel(context.Background(), p)
	if got != etcT.String() || err != nil || len(se.sets) != 1 {
		t.Errorf("Relabel = %q, %v, sets %v", got, err, se.sets)
	}
}

func TestRelabelReturnsTheContextItFailedToSet(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	mustWrite(t, p, "x")
	se := enabledSE(p, tmpT)
	se.setErr = errors.New("denied")
	withSELinux(t, se)
	ctx, logs := flatLogs(t)
	got, err := Relabel(ctx, p)
	if got != etcT.String() || !errors.Is(err, se.setErr) {
		t.Errorf("Relabel = %q, %v", got, err)
	}
	if len(*logs) != 0 {
		t.Errorf("Relabel logged %+v", *logs)
	}
}

func TestRelabelNoSupport(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	mustWrite(t, p, "x")
	for name, se := range map[string]*fakeSE{
		"disabled":          {},
		"unsupported label": {on: true, supported: false, tool: true},
	} {
		t.Run(name, func(t *testing.T) {
			withSELinux(t, se)
			ctx, logs := flatLogs(t)
			got, err := Relabel(ctx, p)
			if got != "" || err != nil || len(*logs) != 0 || len(se.sets) != 0 || len(se.lookups) != 0 {
				t.Errorf("Relabel = %q, %v, logs %+v, sets %v, lookups %v", got, err, *logs, se.sets, se.lookups)
			}
			if c, ok := DefaultContext(ctx, p); c != "" || ok {
				t.Errorf("DefaultContext = %q, %v", c, ok)
			}
		})
	}
}

func TestDefaultContext(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	mustWrite(t, p, "x")
	se := enabledSE(p, tmpT)
	withSELinux(t, se)
	if c, ok := DefaultContext(context.Background(), p); c != etcT.String() || !ok {
		t.Errorf("DefaultContext = %q, %v", c, ok)
	}
	if len(se.lookups) != 1 || se.lookups[0]&0o170000 != 0o100000 {
		t.Errorf("lookup modes = %o, want the file's st_mode", se.lookups)
	}
	se.defErr = errNilHandle
	if c, ok := DefaultContext(context.Background(), p); c != "" || ok {
		t.Errorf("a failed lookup = %q, %v, want no default", c, ok)
	}
	se.defErr, se.def = nil, seContext{}
	if c, ok := DefaultContext(context.Background(), p); c != "" || ok {
		t.Errorf("no default = %q, %v", c, ok)
	}
}

func TestDefaultContextOfAMissingPathLooksUpMode0(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gone")
	se := enabledSE(p, seContext{})
	withSELinux(t, se)
	if c, ok := DefaultContext(context.Background(), p); c != etcT.String() || !ok {
		t.Errorf("DefaultContext = %q, %v", c, ok)
	}
	if len(se.lookups) != 1 || se.lookups[0] != 0 {
		t.Errorf("lookup modes = %v, want [0]", se.lookups)
	}
}
