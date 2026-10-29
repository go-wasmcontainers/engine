package portmap

import "testing"

// FuzzParseSpec: a `-p` value and an API binding come from whoever holds
// the socket. Neither may panic, and what is accepted must be a valid,
// bounded map.
func FuzzParseSpec(f *testing.F) {
	for _, s := range []string{"80", "8080:80", "127.0.0.1::53/udp", "[::1]:8080:80", "8000-8010:80", "1-1024", "a:b:c:d:e", "[", "::"} {
		f.Add(s, "127.0.0.1", "8080-8090")
	}
	f.Fuzz(func(t *testing.T, spec, hostIP, hostPort string) {
		if m, err := ParseSpec(spec); err == nil {
			if m.Bindings() == 0 || m.Bindings() > MaxBindings {
				t.Fatalf("%q: %d bindings", spec, m.Bindings())
			}
			for k, bs := range m {
				if k.Port == 0 || (k.Proto != "tcp" && k.Proto != "udp") {
					t.Fatalf("%q: key %v", spec, k)
				}
				for _, b := range bs {
					if b.HostPortEnd < b.HostPort || !b.Addr().IsValid() {
						t.Fatalf("%q: binding %+v", spec, b)
					}
				}
			}
		}
		if b, err := ParseBinding(hostIP, hostPort); err == nil {
			if b.HostPortEnd < b.HostPort || !b.Addr().IsValid() {
				t.Fatalf("%q %q: %+v", hostIP, hostPort, b)
			}
		}
		ParseKey(spec)
	})
}
