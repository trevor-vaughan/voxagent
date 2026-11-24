package node

import (
	"os"
	"path/filepath"
	"testing"
)

// Measured against Puppet 8.10.0, settings.rb default_certname:
//
//	hostname = facter networking.hostname
//	domain   = facter networking.domain
//	fqdn     = domain.empty? ? hostname : "#{hostname}.#{domain}"
//	fqdn.downcase.sub(/\.$/, '')
//
// The report's host field is this value, so getting it wrong mislabels every
// report a node ever files.
func TestCertnameFrom(t *testing.T) {
	for _, tt := range []struct {
		name     string
		hostname string
		resolv   string
		want     string
	}{
		{
			// An already-qualified hostname carries its own domain, and Facter
			// splits rather than appending -- otherwise the domain lands twice.
			name:     "hostname already qualified",
			hostname: "box.example.com",
			resolv:   "search ignored.invalid\n",
			want:     "box.example.com",
		},
		{
			// The measured case on this machine: a short hostname plus the first
			// search entry. `puppet config print certname` agrees.
			name:     "short hostname takes the first search entry",
			hostname: "cfd81b2d9db2",
			resolv:   "nameserver 1.1.1.1\nsearch cheese.gromit ez.cheese.gromit\n",
			want:     "cfd81b2d9db2.cheese.gromit",
		},
		{
			// RFC 1034 makes domain and search mutually exclusive; domain is the
			// explicit statement of the one thing search only implies.
			name:     "domain beats search",
			hostname: "box",
			resolv:   "search wrong.invalid\ndomain right.example\n",
			want:     "box.right.example",
		},
		{
			// No domain anywhere is not an error. Puppet's own branch is
			// `if domain and domain != ""`, and the bare hostname is the answer.
			name:     "no domain leaves the hostname bare",
			hostname: "box",
			resolv:   "nameserver 1.1.1.1\n",
			want:     "box",
		},
		{
			name:     "trailing dot is stripped",
			hostname: "box",
			resolv:   "domain example.com.\n",
			want:     "box.example.com",
		},
		{
			name:     "the result is downcased",
			hostname: "BOX",
			resolv:   "domain Example.COM\n",
			want:     "box.example.com",
		},
		{
			// A missing resolv.conf is the common case in a minimal container.
			name:     "an unreadable resolv.conf is not an error",
			hostname: "box",
			resolv:   "",
			want:     "box",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "resolv.conf")
			if tt.resolv != "" {
				if err := os.WriteFile(path, []byte(tt.resolv), 0o644); err != nil {
					t.Fatalf("seed resolv.conf: %v", err)
				}
			}
			if got := certnameFrom(tt.hostname, path); got != tt.want {
				t.Errorf("certnameFrom(%q) = %q, want %q", tt.hostname, got, tt.want)
			}
		})
	}
}

// Certname must not return the empty string: it becomes the report's host, and
// an empty host is a report nobody can attribute.
func TestCertnameIsNeverEmpty(t *testing.T) {
	got, err := Certname()
	if err != nil {
		t.Fatalf("Certname returned error: %v", err)
	}
	if got == "" {
		t.Error("Certname returned the empty string")
	}
}
