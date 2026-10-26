package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-wasmcontainers/engine/netdriver/relaynet"
)

// sessionFile keeps the relay session token across restarts, in the data
// root, readable by the daemon's user only.
func sessionFile(cfg Config) string { return filepath.Join(cfg.DataRoot, "relay-session") }

// readSecret reads a small secret file, refusing one others may read.
func readSecret(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is readable by others (mode %v): chmod 600 it", path, st.Mode().Perm())
	}
	if st.Size() > 4096 {
		return "", fmt.Errorf("%s: too large for a secret", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func relayClient(cfg Config) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.RelayCA != "" {
		pem, err := os.ReadFile(cfg.RelayCA)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("--relay-ca %s: no certificate", cfg.RelayCA)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: tr, Timeout: 0}, nil
}

// joinRelay connects to the class: with the session kept from the last
// run, else by joining with the code. The new token is kept (0600) so a
// restart keeps the same addresses.
func joinRelay(ctx context.Context, cfg Config, out io.Writer) (*relaynet.Joined, error) {
	hc, err := relayClient(cfg)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var j *relaynet.Joined
	if tok, err := readSecret(sessionFile(cfg)); err == nil && tok != "" {
		j, err = relaynet.Connect(cctx, cfg.Relay, tok, hc)
		if err != nil {
			fmt.Fprintln(out, "wctrd: relay: the kept session no longer works:", err)
		}
	}
	if j == nil {
		if cfg.RelayCodeFile == "" {
			return nil, errors.New("--relay needs --relay-join-code-file (no kept session)")
		}
		code, err := readSecret(cfg.RelayCodeFile)
		if err != nil {
			return nil, fmt.Errorf("--relay-join-code-file: %w", err)
		}
		host, _ := os.Hostname()
		if j, err = relaynet.Join(cctx, cfg.Relay, code, host, hc); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(cfg.DataRoot, 0o700); err != nil {
		j.Close()
		return nil, err
	}
	if err := os.WriteFile(sessionFile(cfg), []byte(j.Token()+"\n"), 0o600); err != nil {
		j.Close()
		return nil, err
	}
	w := j.Session().Welcome()
	fmt.Fprintf(out, "wctrd: joined the class network %q through %s (session %s)\n", cfg.RelayNetwork, cfg.Relay, w.GetSession().GetId())
	return j, nil
}
