package container

import (
	"strings"
	"testing"
)

func TestCheckAttachments(t *testing.T) {
	got, err := checkAttachments([]Attachment{{Network: "front"}, {Network: "back", Aliases: []string{"API", "db"}}})
	if err != nil || len(got) != 2 || got[1].Aliases[0] != "api" {
		t.Fatalf("%+v, %v", got, err)
	}
	for _, bad := range [][]Attachment{
		{{Network: "a"}, {Network: "a"}},
		{{Network: NoNetwork}, {Network: "a"}},
		{{Network: NoNetwork, Aliases: []string{"x"}}},
		{{Network: "a b"}},
		{{Network: "a", Aliases: []string{"under_score"}}},
		make([]Attachment, MaxAttachments+1),
	} {
		if _, err := checkAttachments(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// TestRecordNetworks: a record written before Networks (one "network")
// reads as that network; a record with both, or a bad one, is refused.
func TestRecordNetworks(t *testing.T) {
	base := record{ID: strings.Repeat("a", 64), Name: "web", State: Created, Cmd: []string{"x"}}
	old := base
	old.Network = "lab"
	if err := old.check(); err != nil || len(old.Networks) != 1 || old.Networks[0].Network != "lab" || old.Network != "" {
		t.Fatalf("old record: %+v, %v", old, err)
	}
	both := base
	both.Network, both.Networks = "lab", []Attachment{{Network: "lab"}}
	if err := both.check(); err == nil {
		t.Error("a record with network and networks accepted")
	}
	dup := base
	dup.Networks = []Attachment{{Network: "lab"}, {Network: "lab"}}
	if err := dup.check(); err == nil {
		t.Error("a record with a network twice accepted")
	}
}
