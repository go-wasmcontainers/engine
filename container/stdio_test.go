package container

import (
	"bytes"
	"testing"
)

func TestParseDetachKeys(t *testing.T) {
	for in, want := range map[string][]byte{
		"":                      {16, 17},
		"ctrl-p,ctrl-q":         {16, 17},
		"ctrl-@":                {0},
		"ctrl-a, ctrl-Z":        {1, 26},
		"ctrl-[,ctrl-\\,ctrl-]": {27, 28, 29},
		"ctrl-^,ctrl-_":         {30, 31},
		"a,b":                   {'a', 'b'},
		"DEL":                   {127},
	} {
		got, err := ParseDetachKeys(in)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%q: %v %v, want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"ctrl-1", "ctrl-", "ctrl-ab", "abc", ",", "a,,b", "a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p,q"} {
		if got, err := ParseDetachKeys(bad); err == nil {
			t.Errorf("%q accepted: %v", bad, got)
		}
	}
}

// TestDetachScanner: input passes through except the sequence; a partial
// match that fails is passed on; the match may span reads.
func TestDetachScanner(t *testing.T) {
	type step struct {
		in, out string
		done    bool
	}
	for name, steps := range map[string][]step{
		"plain":        {{"hello", "hello", false}},
		"whole":        {{"ab\x10\x11cd", "ab", true}},
		"split":        {{"ab\x10", "ab", false}, {"\x11zz", "", true}},
		"false start":  {{"a\x10b", "a\x10b", false}},
		"held then no": {{"a\x10", "a", false}, {"b", "\x10b", false}},
		"repeat first": {{"\x10\x10\x11", "\x10", true}},
	} {
		d := detachScanner{keys: []byte{16, 17}}
		for i, s := range steps {
			out, done := d.scan([]byte(s.in))
			if string(out) != s.out || done != s.done {
				t.Errorf("%s step %d: %q %v, want %q %v", name, i, out, done, s.out, s.done)
			}
		}
	}
	d := detachScanner{}
	if out, done := d.scan([]byte("\x10\x11")); string(out) != "\x10\x11" || done {
		t.Error("no keys: not a pass-through")
	}
}

// FuzzDetach: any keys string and any input, split anywhere. Bad keys
// are refused, never half-parsed. Without the sequence, everything
// typed is passed on in order. With it, what is passed on is a prefix of
// the input that does not itself contain the sequence.
func FuzzDetach(f *testing.F) {
	f.Add("ctrl-p,ctrl-q", []byte("ab\x10\x11cd"), uint8(3))
	f.Add("a,a,b", []byte("aaab"), uint8(1))
	f.Fuzz(func(t *testing.T, keys string, in []byte, split uint8) {
		k, err := ParseDetachKeys(keys)
		if err != nil {
			return
		}
		if len(k) == 0 || len(k) > 16 {
			t.Fatalf("%q parsed to %d keys", keys, len(k))
		}
		d := detachScanner{keys: k}
		cut := int(split) % (len(in) + 1)
		var out []byte
		done := false
		for _, part := range [][]byte{in[:cut], in[cut:]} {
			o, dn := d.scan(part)
			out = append(out, o...)
			if dn {
				done = true
				break
			}
		}
		if bytes.Contains(out, k) {
			t.Fatalf("the sequence was passed on: %q", out)
		}
		if !done {
			// Only a held partial match may be missing at the end.
			if !bytes.HasPrefix(in, out) || len(in)-len(out) >= len(k) {
				t.Fatalf("lost input: %q from %q", out, in)
			}
			return
		}
		if !bytes.Contains(in, k) {
			t.Fatalf("detached on %q without the sequence %q", in, k)
		}
	})
}
