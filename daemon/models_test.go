package daemon

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/go-wasmcontainers/engine/container"
	"github.com/go-wasmcontainers/engine/models"
	"github.com/go-wasmcontainers/engine/rootfs"
)

type fakeSrc struct{ b []byte }

func (f fakeSrc) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(f.b)) {
		return 0, io.EOF
	}
	return copy(p, f.b[off:]), nil
}
func (f fakeSrc) Size() int64  { return int64(len(f.b)) }
func (f fakeSrc) Close() error { return nil }

func TestParseBindModel(t *testing.T) {
	name := models.Registry[0].Name
	m, err := ParseBind(ModelPrefix + name + ":/models:ro")
	if err != nil || !m.ReadOnly || m.Target != "/models" || m.Volume != ModelPrefix+name {
		t.Fatalf("%+v %v", m, err)
	}
	if m, err := ParseBind(ModelPrefix + name + ":/models"); err != nil || !m.ReadOnly {
		t.Fatalf("a model mount is read-only without being told: %+v %v", m, err)
	}
	for _, bad := range []string{ModelPrefix + name + ":/models:rw", ModelPrefix + "nope:/models:ro"} {
		if _, err := ParseBind(bad); !errors.Is(err, container.ErrInvalid) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestModelFS(t *testing.T) {
	e := models.Registry[0]
	var opened []string
	fsys, err := modelFS(func(e models.Entry) (models.Source, error) {
		opened = append(opened, e.Name)
		return fakeSrc{bytes.Repeat([]byte{1}, 10)}, nil
	}, e.Name)
	if err != nil {
		t.Fatal(err)
	}
	st, err := fsys.Stat("/" + e.FileName())
	if err != nil || st.Size() != 10 || len(opened) != 1 {
		t.Fatalf("%v %v %v", st, err, opened)
	}
	if _, err := fsys.OpenFile("/"+e.FileName(), rootfs.O_WRONLY, 0); err == nil {
		t.Fatal("writable model")
	}
	if _, err := modelFS(nil, "nope"); !errors.Is(err, models.ErrUnknown) {
		t.Fatal(err)
	}
	// A source that fails verification never becomes a mount: the opener is
	// where verification happens, and its error is the mount's.
	_, err = modelFS(func(models.Entry) (models.Source, error) { return nil, models.ErrChecksum }, e.Name)
	if !errors.Is(err, models.ErrChecksum) || strings.Contains(err.Error(), "<nil>") {
		t.Fatal(err)
	}
}
