// Package node answers questions about the machine voxagent is running on.
package node

import (
	"bufio"
	"os"
	"strings"
)

// resolvConf is where the domain is looked up when the hostname is not already
// qualified. A variable so the test can point at a fixture.
const resolvConf = "/etc/resolv.conf"

// Certname returns the node's certname, which is the report's host field.
//
// Measured against Puppet 8.10.0, Puppet::Settings.default_certname:
//
//	hostname = facter networking.hostname
//	domain   = facter networking.domain
//	fqdn     = (domain && domain != "") ? "#{hostname}.#{domain}" : hostname
//	fqdn.to_s.downcase.gsub(/\.$/, '')
//
// Ruby reports this even when applying a catalog compiled for another node, so
// a report names the machine that applied it rather than the node the catalog
// describes. voxagent matches that: drop-in compatibility is the contract, and
// a consumer keying reports by host must not see two conventions.
func Certname() (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", err
	}
	return certnameFrom(hostname, resolvConf), nil
}

// certnameFrom is the pure half, so the domain lookup can be tested against a
// fixture rather than against whatever this machine's resolver happens to say.
func certnameFrom(hostname, resolvPath string) string {
	// Facter's networking.hostname is the short name and networking.domain is
	// the rest. Splitting an already-qualified hostname rather than appending to
	// it is what stops the domain being added twice.
	host, domain, qualified := strings.Cut(hostname, ".")
	if !qualified {
		domain = domainFromResolvConf(resolvPath)
	}

	fqdn := host
	if domain != "" {
		fqdn = host + "." + domain
	}
	return strings.TrimSuffix(strings.ToLower(fqdn), ".")
}

// domainFromResolvConf reads the local domain out of resolv.conf.
//
// `domain` wins over `search`: RFC 1034 makes them mutually exclusive, and
// `domain` states the one thing `search` only implies. Only the first entry of
// a `search` list is the local domain -- the rest are additional suffixes to
// try, and treating one of those as the node's own domain would rename it.
//
// An unreadable or absent file yields no domain rather than an error. That is
// the ordinary state of a minimal container, and Puppet's own branch treats an
// empty domain as "use the bare hostname" rather than as a failure.
func domainFromResolvConf(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()

	domain := ""
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		switch fields[0] {
		case "domain":
			return fields[1]
		case "search":
			// Kept, not returned: a later `domain` line still outranks it.
			if domain == "" {
				domain = fields[1]
			}
		}
	}
	// A read that stopped early may have stopped before a `domain` line that
	// would have outranked the `search` entry above. Reporting the search entry
	// anyway would name the node from a file we did not finish reading, so treat
	// a truncated read as no answer -- the same as an absent file.
	if scanner.Err() != nil {
		return ""
	}
	return domain
}
