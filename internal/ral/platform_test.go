package ral

import "testing"

func TestRealPlatformOSRelease(t *testing.T) {
	if _, err := (realPlatform{}).OSRelease(); err != nil {
		t.Skipf("no /etc/os-release: %v", err)
	}
}
