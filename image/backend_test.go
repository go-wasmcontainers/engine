package image_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/go-wasmcontainers/engine/image"
	"github.com/go-wasmcontainers/engine/internal/testutil"
)

func TestMemoryBackend(t *testing.T) {
	c := testutil.BuildContent(testutil.ImageSpec{RefName: "a", Module: fakeModule, Index: true})
	dir := filepath.Join(t.TempDir(), "l")
	testutil.WriteLayout(t, dir, c)
	b := image.NewMemoryBackend(0)
	st, err := image.NewStore(b)
	if err != nil {
		t.Fatal(err)
	}
	src, name, _ := image.OpenSource("oci:"+dir+":a", image.RegistryOptions{}, image.DefaultLimits)
	res, err := image.Pull(context.Background(), st, src, image.DefaultLimits, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Tag(name, image.Ref{Manifest: res.Image.ManifestDigest, RepoDigest: res.RepoDigest}); err != nil {
		t.Fatal(err)
	}
	// The name table survives a "reopen" over the same backend.
	st2, err := image.NewStore(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st2.Resolve(name); !ok {
		t.Fatal("refs lost")
	}
	if img, err := st2.Image(res.Image.ManifestDigest, image.DefaultLimits); err != nil || img.Size == 0 {
		t.Fatal(img, err)
	}
	st2.Untag(name)
	gone, err := st2.GC(image.DefaultLimits)
	if err != nil || len(gone) == 0 {
		t.Fatal(gone, err)
	}
	if left, _ := b.ListBlobs(); len(left) != 0 {
		t.Fatal(left)
	}

	// A failed (unverified) put leaves nothing behind.
	blob := []byte("blob")
	d := image.Descriptor{Digest: image.FromBytes(blob), Size: 4}
	if err := st2.Ingest(d, bytes.NewReader([]byte("blub"))); !errors.Is(err, image.ErrDigest) {
		t.Fatal(err)
	}
	if st2.Has(d.Digest) {
		t.Fatal("unverified blob visible")
	}
	if err := b.PutBlob("nothex", bytes.NewReader(nil)); err == nil {
		t.Fatal("bad name accepted")
	}
	if _, err := b.OpenBlob(d.Digest.Encoded()); !errors.Is(err, image.ErrNotFound) {
		t.Fatal(err)
	}
	// A bounded backend refuses what does not fit.
	small := image.NewMemoryBackend(3)
	ss, _ := image.NewStore(small)
	if err := ss.Ingest(d, bytes.NewReader(blob)); !errors.Is(err, image.ErrTooLarge) {
		t.Fatal(err)
	}
	ok := image.NewMemoryBackend(4)
	os, _ := image.NewStore(ok)
	if err := os.Ingest(d, bytes.NewReader(blob)); err != nil {
		t.Fatal(err)
	}
	rc, _ := os.Open(d.Digest)
	got, _ := io.ReadAll(rc)
	if string(got) != "blob" {
		t.Fatal(string(got))
	}
	ok.DeleteBlob(d.Digest.Encoded())
	if ok.HasBlob(d.Digest.Encoded()) {
		t.Fatal("delete")
	}
	if _, err := image.NewStore(badRefs{image.NewMemoryBackend(0)}); err == nil {
		t.Fatal("corrupt refs accepted")
	}
}

type badRefs struct{ *image.MemoryBackend }

func (badRefs) LoadRefs() ([]byte, error) { return []byte("{"), nil }
