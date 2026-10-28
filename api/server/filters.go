package server

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// maxFilterValues bounds the values of one filters parameter.
const maxFilterValues = 256

// parseFilters reads Docker's filters parameter, in either of its forms:
// {"key":["v",...]} or the older {"key":{"v":true}}. A key outside allowed
// is refused, not ignored: a filter that is not applied answers with more
// than was asked for.
func parseFilters(raw string, allowed ...string) (map[string][]string, error) {
	out := map[string][]string{}
	if raw == "" {
		return out, nil
	}
	var generic map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		return nil, fmt.Errorf("invalid filters: %v", err)
	}
	n := 0
	for k, v := range generic {
		if !slices.Contains(allowed, k) {
			return nil, fmt.Errorf("invalid filter %q", k)
		}
		var list []string
		if err := json.Unmarshal(v, &list); err != nil {
			var set map[string]bool
			if err := json.Unmarshal(v, &set); err != nil {
				return nil, fmt.Errorf("invalid filter %q: want a list of strings", k)
			}
			for s, on := range set {
				if on {
					list = append(list, s)
				}
			}
			sort.Strings(list)
		}
		if n += len(list); n > maxFilterValues {
			return nil, fmt.Errorf("invalid filters: more than %d values", maxFilterValues)
		}
		out[k] = list
	}
	return out, nil
}

// labelsMatch applies label filters, all of which must hold: "k" (the
// label is set), "k=v" (set to v), "k!=v" (not set to v).
func labelsMatch(labels map[string]string, terms []string) bool {
	for _, t := range terms {
		if k, v, ok := strings.Cut(t, "!="); ok {
			if got, has := labels[k]; has && got == v {
				return false
			}
			continue
		}
		k, v, hasValue := strings.Cut(t, "=")
		got, has := labels[k]
		if !has || (hasValue && got != v) {
			return false
		}
	}
	return true
}

// anyMatch is true when vals is empty or ok holds for one of them.
func anyMatch(vals []string, ok func(string) bool) bool {
	if len(vals) == 0 {
		return true
	}
	return slices.ContainsFunc(vals, ok)
}
