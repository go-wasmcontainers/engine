package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/internal/testutil"
)

func demuxSeeds() []any {
	var b bytes.Buffer
	api.WriteFrame(&b, 1, []byte("hello\n"))
	api.WriteFrame(&b, 2, []byte("err\n"))
	return []any{b.Bytes(), []byte{1, 0, 0, 0, 0xff, 0xff, 0xff, 0xff}, []byte{3, 0, 0, 0, 0, 0, 0, 0}, []byte{}}
}

// FuzzDemux: the client parses whatever the daemon sends. It must never
// panic, never write more than it was sent, and refuse what is malformed.
func FuzzDemux(f *testing.F) {
	for _, s := range demuxSeeds() {
		f.Add(s.([]byte))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var o, e bytes.Buffer
		api.Demux(&o, &e, bytes.NewReader(b))
		if o.Len()+e.Len() > len(b) {
			t.Fatal("wrote more than it read")
		}
		var v struct{ A api.StrSlice }
		json.Unmarshal(b, &v)
		var m api.JSONMessage
		json.Unmarshal(b, &m)
		// What WriteFrame writes, Demux reads back.
		var round bytes.Buffer
		api.WriteFrame(&round, api.StreamStderr, b)
		var got bytes.Buffer
		if err := api.Demux(io.Discard, &got, &round); err != nil || !bytes.Equal(got.Bytes(), b) {
			t.Fatalf("round trip: %v", err)
		}
	})
}

func TestWriteFuzzCorpus(t *testing.T) {
	testutil.WriteCorpus(t, "FuzzDemux", demuxSeeds()...)
}

func TestCorpusIsCommitted(t *testing.T) {
	ents, err := os.ReadDir(filepath.Join("testdata", "fuzz", "FuzzDemux"))
	if err != nil || len(ents) == 0 {
		t.Fatalf("no committed corpus: %v", err)
	}
}
