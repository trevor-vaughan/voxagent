package raltest

// FakePlatform is a ral.Platform answering from fixed data.
type FakePlatform struct {
	Exec    map[string]bool
	Files   map[string]bool
	Release string
	Err     error
}

func (f FakePlatform) Executable(path string) bool { return f.Exec[path] }
func (f FakePlatform) FileExists(path string) bool { return f.Files[path] }
func (f FakePlatform) OSRelease() ([]byte, error) {
	return []byte(f.Release), f.Err
}

// CentOS10 is the /etc/os-release of a CentOS Stream 10 host.
const CentOS10 = "NAME=\"CentOS Stream\"\nID=\"centos\"\nID_LIKE=\"rhel fedora\"\nVERSION_ID=\"10\"\n"
