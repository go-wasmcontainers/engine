package daemon

import (
	"reflect"
	"strings"
	"testing"

	"github.com/go-wasmcontainers/engine/image"
)

func TestApplyChanges(t *testing.T) {
	base := image.ExecConfig{Env: []string{"A=0", "PATH=/bin"}, Labels: map[string]string{"k": "v"}}
	got, err := ApplyChanges(base, []string{
		`ENV A=1 B="two words"`,
		`ENV LEGACY old form`,
		`CMD ["cat", "/data.txt"]`,
		`ENTRYPOINT /bin/app --flag`,
		`WORKDIR /srv`,
		`USER 1000`,
		`LABEL a=b c="d e"`,
		`EXPOSE 80 53/udp`,
		`VOLUME ["/data", "/logs"]`,
		`STOPSIGNAL SIGTERM`,
		`ONBUILD RUN make`,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := image.ExecConfig{
		Env:          []string{"A=1", "PATH=/bin", "B=two words", "LEGACY=old form"},
		Cmd:          []string{"cat", "/data.txt"},
		Entrypoint:   []string{"/bin/sh", "-c", "/bin/app --flag"},
		WorkingDir:   "/srv",
		User:         "1000",
		Labels:       map[string]string{"k": "v", "a": "b", "c": "d e"},
		ExposedPorts: map[string]struct{}{"80/tcp": {}, "53/udp": {}},
		Volumes:      map[string]struct{}{"/data": {}, "/logs": {}},
		StopSignal:   "SIGTERM",
		OnBuild:      []string{"RUN make"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	// The base is not changed in place.
	if base.Env[0] != "A=0" || len(base.Labels) != 1 {
		t.Fatalf("base changed: %+v", base)
	}
	for _, bad := range [][]string{
		{"RUN make"}, {"FROM x"}, {"CMD"}, {`CMD ["unterminated`}, {"WORKDIR"}, {`LABEL nokey`},
		{`ENV A="open`}, {strings.Repeat("x", 5000)}, make([]string, MaxChanges+1),
	} {
		if _, err := ApplyChanges(base, bad); err == nil {
			t.Errorf("%.40q accepted", bad)
		}
	}
}

func FuzzApplyChanges(f *testing.F) {
	f.Add(`ENV A=1 B="x y"`)
	f.Add(`CMD ["a"]`)
	f.Add(`LABEL a=b`)
	f.Add(`VOLUME ["/x"]`)
	f.Fuzz(func(t *testing.T, ch string) {
		c, err := ApplyChanges(image.ExecConfig{}, []string{ch})
		if err != nil {
			return
		}
		for _, kv := range c.Env {
			if !strings.Contains(kv, "=") {
				t.Fatalf("env entry %q", kv)
			}
		}
	})
}
