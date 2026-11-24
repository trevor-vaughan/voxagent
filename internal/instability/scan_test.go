package instability

import (
	"strings"
	"testing"
)

func kinds(fs []Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Kind)
	}
	return out
}

func hasKind(fs []Finding, kind string) bool {
	for _, f := range fs {
		if f.Kind == kind {
			return true
		}
	}
	return false
}

func TestScanManifestFindsExportedResource(t *testing.T) {
	src := []byte("class a {\n  @@nagios_host { 'web01': ensure => present }\n}\n")
	got, err := ScanManifest("a.pp", src)
	if err != nil {
		t.Fatalf("ScanManifest returned error: %v", err)
	}
	if !hasKind(got, KindExportedResource) {
		t.Fatalf("exported resource not flagged; got %v", kinds(got))
	}
	if got[0].Line != 2 {
		t.Errorf("Line = %d, want 2", got[0].Line)
	}
}

func TestScanManifestFindsExportedCollector(t *testing.T) {
	src := []byte("class a {\n  Nagios_host <<| |>>\n}\n")
	got, err := ScanManifest("a.pp", src)
	if err != nil {
		t.Fatalf("ScanManifest returned error: %v", err)
	}
	if !hasKind(got, KindExportedCollector) {
		t.Fatal("exported collector not flagged")
	}
}

func TestScanManifestFindsGenerateCall(t *testing.T) {
	src := []byte("class a {\n  $x = generate('/bin/date')\n}\n")
	got, err := ScanManifest("a.pp", src)
	if err != nil {
		t.Fatalf("ScanManifest returned error: %v", err)
	}
	if !hasKind(got, KindGenerateCall) {
		t.Fatal("generate() call not flagged")
	}
}

// A virtual resource is not exported and does not leave the compiler.
func TestScanManifestIgnoresVirtualResource(t *testing.T) {
	src := []byte("class a {\n  @user { 'bob': ensure => present }\n}\n")
	got, err := ScanManifest("a.pp", src)
	if err != nil {
		t.Fatalf("ScanManifest returned error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("virtual resource flagged: %v", kinds(got))
	}
}

func TestScanManifestIgnoresComments(t *testing.T) {
	src := []byte("class a {\n  # @@nagios_host is not declared here\n  # generate() neither\n}\n")
	got, err := ScanManifest("a.pp", src)
	if err != nil {
		t.Fatalf("ScanManifest returned error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("commented constructs flagged: %v", kinds(got))
	}
}

func TestScanManifestIgnoresCleanManifest(t *testing.T) {
	src := []byte("class a {\n  file { '/tmp/x': ensure => file }\n}\n")
	got, err := ScanManifest("a.pp", src)
	if err != nil {
		t.Fatalf("ScanManifest returned error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("clean manifest flagged: %v", kinds(got))
	}
}

func TestScanRubyFunctionFindsIO(t *testing.T) {
	cases := map[string]string{
		"File.read": "  File.read('/etc/hosts')\n",
		"IO.popen":  "  IO.popen('ls')\n",
		"backticks": "  x = `hostname`\n",
		"system":    "  system('true')\n",
		"Net::HTTP": "  Net::HTTP.get(uri)\n",
		"Open3":     "  Open3.capture2('ls')\n",
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			src := []byte("Puppet::Functions.create_function(:f) do\n" + line + "end\n")
			got, err := ScanRubyFunction("f.rb", src)
			if err != nil {
				t.Fatalf("ScanRubyFunction returned error: %v", err)
			}
			if !hasKind(got, KindFunctionIO) {
				t.Errorf("%s not flagged as I/O", name)
			}
		})
	}
}

func TestScanRubyFunctionIgnoresPureFunction(t *testing.T) {
	src := []byte("Puppet::Functions.create_function(:f) do\n  args[0].upcase\nend\n")
	got, err := ScanRubyFunction("f.rb", src)
	if err != nil {
		t.Fatalf("ScanRubyFunction returned error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("pure function flagged: %v", kinds(got))
	}
}

func TestScanRubyFunctionIgnoresComments(t *testing.T) {
	src := []byte("# File.read is mentioned in this comment\n")
	got, err := ScanRubyFunction("f.rb", src)
	if err != nil {
		t.Fatalf("ScanRubyFunction returned error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("commented I/O flagged: %v", kinds(got))
	}
}

// A line over bufio.Scanner's 1 MiB buffer must surface as an error, not a
// silent partial scan. Regression test for a manifest with a 2 MiB single-line
// string literal followed by an exported resource: the exported resource must
// never go unreported without any indication the file wasn't fully scanned.
func TestScanManifestReturnsErrorOnOverLongLine(t *testing.T) {
	overLong := strings.Repeat("x", 2*1024*1024)
	src := []byte("$giant = '" + overLong + "'\n@@nagios_host { 'web01': ensure => present }\n")
	_, err := ScanManifest("a.pp", src)
	if err == nil {
		t.Fatal("ScanManifest returned nil error for a file with a line over 1 MiB; want error")
	}
}

func TestScanManifestReturnsNilErrorOnNormalFile(t *testing.T) {
	src := []byte("class a {\n  file { '/tmp/x': ensure => file }\n}\n")
	_, err := ScanManifest("a.pp", src)
	if err != nil {
		t.Fatalf("ScanManifest returned error for a normal multi-line file: %v", err)
	}
}
