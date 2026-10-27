package server

import (
	"net/http/httptest"
	"testing"
)

func TestBodyLimit(t *testing.T) {
	s := &server{}
	for _, c := range []struct {
		method, path string
		want         int64
	}{
		{"POST", "/images/load", DefaultMaxUploadBytes},
		{"PUT", "/containers/abc/archive", DefaultMaxUploadBytes},
		{"GET", "/containers/abc/archive", maxBody},
		{"POST", "/containers/create", maxBody},
		{"POST", "/commit", maxBody},
		{"PUT", "/containers/abc/other", maxBody},
	} {
		if got := s.bodyLimit(httptest.NewRequest(c.method, c.path, nil)); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, got, c.want)
		}
	}
	s.info.MaxUploadBytes = 4096
	if got := s.bodyLimit(httptest.NewRequest("POST", "/images/load", nil)); got != 4096 {
		t.Errorf("configured limit: %d", got)
	}
}
