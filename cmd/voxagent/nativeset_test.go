package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/ral"
)

// The set of native types nativeProviders returns is part of the contract
// (the units restructure must not change it), though not its order: the
// registry is a map. Each row is a fixture catalog and platform with the
// sorted native types captured before any type moved into a unit.
func TestNativeTypeSetUnchanged(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the repository")
	}
	coreLib := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(here))), "internal", "units", "augeas_core", "testdata", "lib")
	const centos10 = "ID=\"centos\"\nID_LIKE=\"rhel fedora\"\nVERSION_ID=\"10\"\n"
	const rhel9 = "ID=\"rhel\"\nVERSION_ID=\"9.4\"\n"
	res := func(types ...string) *catalog.Catalog {
		c := &catalog.Catalog{}
		for _, typ := range types {
			c.Resources = append(c.Resources, catalog.Resource{Type: typ, Title: "t"})
		}
		return c
	}
	for _, tc := range []struct {
		name     string
		cat      *catalog.Catalog
		libdir   func(t *testing.T) string
		platform ral.Platform
		want     []string
	}{
		{"core types and user/group on the measured platform",
			res("Notify", "File", "Exec", "Package", "Service", "User", "Group"),
			func(t *testing.T) string { return t.TempDir() }, fakePlatform{centos10},
			[]string{"exec", "file", "group", "notify", "package", "service", "user"}},
		{"core types alone off the measured platform",
			res("Notify", "File", "Exec", "Package", "Service", "User", "Group"),
			func(t *testing.T) string { return t.TempDir() }, fakePlatform{rhel9},
			[]string{"exec", "file", "notify", "package", "service"}},
		{"concat with a measured tree",
			res("Concat_file", "Concat_fragment"),
			func(t *testing.T) string { return concatLibdir(t, nil) }, fakePlatform{rhel9},
			[]string{"concat_file", "concat_fragment", "exec", "file", "notify", "package", "service"}},
		{"augeasproviders types with a measured tree",
			res("Sysctl", "Ssh_config", "Sshd_config", "User", "Group"),
			func(t *testing.T) string { return augeasLibdir(t) }, augeasPlatform{},
			[]string{"exec", "file", "group", "notify", "package", "service", "ssh_config", "sshd_config",
				"sysctl", "user"}},
		{"augeas with a measured tree",
			&catalog.Catalog{Resources: []catalog.Resource{{Type: "Augeas", Title: "hosts",
				Parameters: map[string]any{"context": "/files/etc/hosts", "changes": "set 1/ipaddr 1.2.3.4"}}}},
			func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.CopyFS(dir, os.DirFS(coreLib)); err != nil {
					t.Fatal(err)
				}
				return dir
			}, augeasPlatform{},
			[]string{"augeas", "exec", "file", "group", "notify", "package", "service", "user"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := applyOptions{command: "apply", libdir: tc.libdir(t), augeasProbe: &fakeAugeasProbe{lib: &fakeAugeasLib{}}, environ: environOf(nil)}
			natives, _ := nativeProviders(tc.cat, o, tc.platform)
			var got []string
			for _, p := range natives {
				got = append(got, strings.ToLower(p.Type()))
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("native types %q, want %q", got, tc.want)
			}
		})
	}
}
