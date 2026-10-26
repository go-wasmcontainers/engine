package wasmbin

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/go-wasmcontainers/engine/internal/testutil"
)

// tiny is (module (memory 1) (export "memory" (memory 0))).
var tiny = []byte{
	0, 'a', 's', 'm', 1, 0, 0, 0,
	5, 3, 1, 0x00, 1, // memory section: 1 memory, flags 0, min 1
	7, 10, 1, 6, 'm', 'e', 'm', 'o', 'r', 'y', 2, 0, // export memory 0
}

func memType(t *testing.T, mod []byte) (min, max uint32, shared bool) {
	t.Helper()
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	defer r.Close(ctx)
	cm, err := r.CompileModule(ctx, mod)
	if err != nil {
		t.Fatal(err)
	}
	mt := cm.ExportedMemories()["memory"]
	if mt == nil {
		t.Fatal("no memory export")
	}
	m, ok := mt.Max()
	if !ok {
		m = 0
	}
	return mt.Min(), m, false
}

func TestLimitMemory(t *testing.T) {
	out, err := LimitMemory(tiny, 16, false)
	if err != nil {
		t.Fatal(err)
	}
	if min, max, _ := memType(t, out); min != 1 || max != 16 {
		t.Fatal(min, max)
	}
	// Shared: flags 0x03 with the same limits.
	out, err = LimitMemory(tiny, 16, true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte{5, 4, 1, 0x03, 1, 16}) {
		t.Fatalf("% x", out)
	}
	// A lower own maximum wins; a minimum above the limit is refused.
	own := append([]byte(nil), tiny...)
	own = append(own[:8], append([]byte{5, 4, 1, 0x01, 1, 4}, own[13:]...)...)
	out, err = LimitMemory(own, 16, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, max, _ := memType(t, out); max != 4 {
		t.Fatal(max)
	}
	big := append([]byte(nil), tiny[:8]...)
	big = append(big, 5, 3, 1, 0x00, 100)
	if _, err := LimitMemory(big, 16, false); err == nil {
		t.Fatal("minimum above the limit accepted")
	}
	for name, mod := range map[string][]byte{
		"no header":     []byte("MZ"),
		"no memory":     tiny[:8],
		"overrun":       append(append([]byte(nil), tiny[:8]...), 5, 99, 1),
		"two memories":  append(append([]byte(nil), tiny[:8]...), 5, 5, 2, 0, 1, 0, 1),
		"two sections":  append(append([]byte(nil), tiny[:13]...), 5, 3, 1, 0, 1),
		"memory64":      append(append([]byte(nil), tiny[:8]...), 5, 3, 1, 0x04, 1),
		"trailing":      append(append([]byte(nil), tiny[:8]...), 5, 4, 1, 0, 1, 9),
		"bad leb":       append(append([]byte(nil), tiny[:8]...), 5, 0xff),
		"huge min":      append(append([]byte(nil), tiny[:8]...), 5, 5, 1, 0x00, 0x81, 0x80, 0x08),
		"memory import": append(append([]byte(nil), tiny[:8]...), 2, 7, 1, 1, 'm', 1, 'm', 2, 0),
	} {
		if _, err := LimitMemory(mod, 16, false); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := LimitMemory(tiny, 0, false); err == nil {
		t.Fatal("zero limit")
	}
	if _, err := LimitMemory(tiny, MaxPages+1, false); err == nil {
		t.Fatal("limit above 4 GiB")
	}
	if !errors.Is(func() error { _, err := LimitMemory([]byte("x"), 1, false); return err }(), ErrInvalid) {
		t.Fatal("not ErrInvalid")
	}
}

// TestRealGuest patches a real Go wasip1 binary and shows the cap bites:
// under wazero's default 4 GiB limit, the patched binary's own maximum
// stops a memory hog.
func TestRealGuest(t *testing.T) {
	mod := testutil.Wasm(t, "probe")
	out, err := LimitMemory(mod, 512, false) // 32 MiB
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	defer r.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, r)
	var stderr bytes.Buffer
	_, err = r.InstantiateWithConfig(ctx, out, wazero.NewModuleConfig().WithArgs("probe", "hog").WithStderr(&stderr))
	if err == nil || !bytes.Contains(stderr.Bytes(), []byte("out of memory")) {
		t.Fatalf("%v %s", err, stderr.String())
	}
	// Imports with tables, globals and functions are walked, not refused.
	if _, err := LimitMemory(mod, 512, true); err != nil {
		t.Fatal(err)
	}
}

func FuzzLimitMemory(f *testing.F) {
	f.Add(tiny, uint32(16))
	f.Add(append(append([]byte(nil), tiny[:8]...), 2, 7, 1, 1, 'm', 1, 'm', 0, 0), uint32(1))
	f.Fuzz(func(t *testing.T, mod []byte, pages uint32) {
		out, err := LimitMemory(mod, pages, pages%2 == 0)
		if err != nil {
			return
		}
		// What it produces it accepts again, unchanged in size class.
		if _, err := LimitMemory(out, pages, pages%2 == 0); err != nil {
			t.Fatalf("output refused: %v", err)
		}
	})
}

func TestWriteFuzzCorpus(t *testing.T) {
	testutil.WriteCorpus(t, "FuzzLimitMemory", testutil.Tuple{tiny, uint32(16)}, testutil.Tuple{[]byte("\x00asm\x01\x00\x00\x00\x02\x07\x01\x01m\x01m\x00\x00"), uint32(1)})
}

func TestCorpusIsCommitted(t *testing.T) {
	ents, err := os.ReadDir(filepath.Join("testdata", "fuzz", "FuzzLimitMemory"))
	if err != nil || len(ents) == 0 {
		t.Fatalf("no committed corpus: %v", err)
	}
}
