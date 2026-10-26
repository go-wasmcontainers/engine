package api

import (
	"os"
	"path/filepath"
)

// DefaultSocket is where dockerd listens and docker connects when neither
// -H nor DOCKER_HOST says otherwise: $XDG_RUNTIME_DIR/wasmcontainers/
// dockerd.sock, else ~/.wasmcontainers/dockerd.sock. It is deliberately not
// /var/run/docker.sock, so the engine never collides with a real Docker.
func DefaultSocket() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "wasmcontainers", "dockerd.sock")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.TempDir()
	}
	return filepath.Join(home, ".wasmcontainers", "dockerd.sock")
}

// DefaultHost is DOCKER_HOST if set, else unix:// + DefaultSocket().
func DefaultHost() string {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return h
	}
	return "unix://" + DefaultSocket()
}
