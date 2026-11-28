package relaynet

import (
	"strings"
	"testing"
)

// FuzzAllowlist: whatever the relay sends, the engine shows only exact,
// lower-case, dotted host names on 443, once each, never more than the bound,
// and never panics.
func FuzzAllowlist(f *testing.F) {
	for _, s := range []string{
		"pypi.org:443", "a.b.example.org:443", "*.example.org:443", "1.2.3.4:443", "[::1]:443",
		"host:80", "host.example.org:0443", "host.example.org:+443", "host.example.org:443:443",
		"Host.Example.org:443", "x.example.org.:443", "\x00.example.org:443", "é.example.org:443", ":", "", "a:b:c",
	} {
		f.Add(s, "pypi.org:443")
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		seen := map[string]bool{}
		got := destsOf([]string{a, b, a})
		if len(got) > maxAllowlist {
			t.Fatalf("%d entries", len(got))
		}
		for _, d := range got {
			if d.Port != 443 {
				t.Fatalf("port %d from %q %q", d.Port, a, b)
			}
			if d.Host == "" {
				t.Fatalf("empty host from %q %q", a, b)
			}
			if d.Host != strings.ToLower(d.Host) || strings.ContainsAny(d.Host, "*:[]/@ \x00") || !strings.Contains(d.Host, ".") {
				t.Fatalf("host %q from %q %q", d.Host, a, b)
			}
			// An IP literal's last label starts with a digit; no TLD does.
			if c := d.Host[strings.LastIndexByte(d.Host, '.')+1]; c >= '0' && c <= '9' {
				t.Fatalf("IP-like host %q", d.Host)
			}
			if seen[d.Host] {
				t.Fatalf("duplicate %q", d.Host)
			}
			seen[d.Host] = true
		}
	})
}

// FuzzOrigin: the origin shown is empty or scheme://host[:port], nothing else.
func FuzzOrigin(f *testing.F) {
	for _, s := range []string{
		"https://relay.example:8443", "https://u:pw@relay.example/x?token=abc#frag", "http://[::1]:80",
		"https://relay.example:99999", "wss://x", "https://", "", "https://a b", "https://%00", "https:///x", "https://@",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		o := originOf(in)
		if o == "" {
			return
		}
		if !strings.HasPrefix(o, "https://") && !strings.HasPrefix(o, "http://") {
			t.Fatalf("scheme: %q from %q", o, in)
		}
		rest := o[strings.Index(o, "://")+3:]
		if rest == "" || strings.ContainsAny(rest, "/?#@ \x00\\") {
			t.Fatalf("origin %q from %q", o, in)
		}
		if originOf(o) != o {
			t.Fatalf("not idempotent: %q -> %q", o, originOf(o))
		}
	})
}
