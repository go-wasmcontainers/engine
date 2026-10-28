package server

import (
	"slices"
	"testing"
)

func TestParseFilters(t *testing.T) {
	f, err := parseFilters(`{"label":["a=b","c"],"name":{"web":true,"off":false}}`, "label", "name")
	if err != nil || !slices.Equal(f["label"], []string{"a=b", "c"}) || !slices.Equal(f["name"], []string{"web"}) {
		t.Fatalf("%v, %v", f, err)
	}
	for _, bad := range []string{`{"other":["x"]}`, `[`, `{"label":"x"}`, `{"label":[1]}`} {
		if _, err := parseFilters(bad, "label", "name"); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	labels := map[string]string{"app": "web", "tier": "front"}
	for _, c := range []struct {
		terms []string
		want  bool
	}{
		{nil, true}, {[]string{"app"}, true}, {[]string{"app=web", "tier=front"}, true},
		{[]string{"app=db"}, false}, {[]string{"nope"}, false}, {[]string{"app=web", "nope"}, false},
		{[]string{"app!=db"}, true}, {[]string{"app!=web"}, false}, {[]string{"nope!=x"}, true},
	} {
		if got := labelsMatch(labels, c.terms); got != c.want {
			t.Errorf("labelsMatch(%v) = %v", c.terms, got)
		}
	}
}

// FuzzParseFilters: any query string parses or is refused, never panics,
// and what parses respects the allowed keys and the value bound.
func FuzzParseFilters(f *testing.F) {
	f.Add(`{"label":["a=b"]}`)
	f.Add(`{"label":{"a":true}}`)
	f.Add(`{"name":["x","y"],"label":[]}`)
	f.Fuzz(func(t *testing.T, raw string) {
		got, err := parseFilters(raw, "label", "name")
		if err != nil {
			return
		}
		n := 0
		for k, v := range got {
			if k != "label" && k != "name" {
				t.Fatalf("key %q passed", k)
			}
			n += len(v)
			labelsMatch(map[string]string{"a": "b"}, v)
		}
		if n > maxFilterValues {
			t.Fatalf("%d values", n)
		}
	})
}
