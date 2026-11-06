package settings

import (
	"fmt"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/runplane/runplane/internal/field"
	"github.com/runplane/runplane/internal/sandbox/allowlist"
	"github.com/runplane/runplane/internal/store"
)

// clean trims and normalizes the values of next that differ from old, and
// returns the problems of these values. The limits of the store.Settings
// tags are for the API to check.
func clean(old store.Settings, next *store.Settings) field.Problems {
	var p field.Problems
	text := func(v *string, before string) bool {
		if *v == before {
			return false
		}
		*v = strings.TrimSpace(*v)
		return true
	}
	if text(&next.InstanceName, old.InstanceName) && next.InstanceName == "" {
		p.Add("instance_name", "Enter a name.", next.InstanceName)
	}
	if text(&next.PublicURL, old.PublicURL) && next.PublicURL != "" {
		u, err := url.Parse(next.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			p.Add("public_url", "Enter a URL such as https://ci.example.com, without a path.", next.PublicURL)
		} else {
			next.PublicURL = u.Scheme + "://" + u.Host
		}
	}
	if text(&next.Domain, old.Domain) {
		next.Domain = strings.ToLower(next.Domain)
		if next.Domain != "" && !validDomain(next.Domain) {
			p.Add("domain", "Enter a domain name such as ci.example.com.", next.Domain)
		}
	}
	if text(&next.ACMEEmail, old.ACMEEmail) && next.ACMEEmail != "" {
		if a, err := mail.ParseAddress(next.ACMEEmail); err != nil || a.Address != next.ACMEEmail {
			p.Add("acme_email", "Enter an email address.", next.ACMEEmail)
		}
	}
	if !slices.Equal(next.EgressAllowlist, old.EgressAllowlist) {
		for i, v := range next.EgressAllowlist {
			v = strings.ToLower(strings.TrimSpace(v))
			next.EgressAllowlist[i] = v
			if _, ok := allowlist.Parse(v); !ok {
				p.Add(fmt.Sprintf("egress_allowlist[%d]", i), "Enter a host name, IPv4 address, or IPv4 CIDR.", v)
			}
		}
	}
	if !slices.Equal(next.DNSServers, old.DNSServers) {
		for i, v := range next.DNSServers {
			v = strings.TrimSpace(v)
			next.DNSServers[i] = v
			if a, err := netip.ParseAddr(v); err != nil || !a.Is4() {
				p.Add(fmt.Sprintf("dns_servers[%d]", i), "Enter an IPv4 address.", v)
			}
		}
	}
	b, was := &next.BackupS3, old.BackupS3
	if text(&b.Endpoint, was.Endpoint) && b.Endpoint != "" {
		u, err := url.Parse(b.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			p.Add("backup_s3.endpoint", "Enter a URL such as https://s3.example.com.", b.Endpoint)
		}
	}
	if text(&b.Region, was.Region) && !s3Region.MatchString(b.Region) {
		p.Add("backup_s3.region", "Enter a region such as us-east-1.", b.Region)
	}
	if text(&b.Bucket, was.Bucket) && b.Bucket != "" && !s3Bucket.MatchString(b.Bucket) {
		p.Add("backup_s3.bucket", "Use 3 to 63 lowercase letters, digits, dots, and dashes.", b.Bucket)
	}
	if text(&b.Prefix, was.Prefix) {
		b.Prefix = strings.TrimPrefix(b.Prefix, "/")
	}
	text(&b.AccessKey, was.AccessKey)
	return p
}

var (
	s3Region = regexp.MustCompile(`^[a-z0-9-]*$`)
	s3Bucket = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// validDomain accepts a lowercase host name with at least two labels, not an
// IP address.
func validDomain(d string) bool {
	labels := strings.Split(d, ".")
	if len(d) > 253 || len(labels) < 2 || net.ParseIP(d) != nil {
		return false
	}
	for _, l := range labels {
		if !dnsLabel.MatchString(l) {
			return false
		}
	}
	return true
}
