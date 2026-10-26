// Package relaynet puts containers on a class network through the relay
// (github.com/go-wasmcontainers/relay): one driver.Session per engine, one
// endpoint per container, as the relay's docs/NETWORKDRIVER.md describes.
// Addresses and names are the relay's ("<container>", or
// "<container>.<student>.<domain>"); the firewall is the teacher's.
package relaynet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/go-wasmcontainers/relay/driver"
	v1 "github.com/go-wasmcontainers/relay/gen/relayv1"
	"github.com/go-wasmcontainers/relay/gen/relayv1/relayv1connect"

	"github.com/go-wasmcontainers/engine/netdriver"
)

// Driver is a NetworkDriver over one relay session.
type Driver struct {
	s *driver.Session
}

// New wraps a connected session.
func New(s *driver.Session) *Driver { return &Driver{s: s} }

// Session is the relay session.
func (d *Driver) Session() *driver.Session { return d.s }

// Attach implements netdriver.NetworkDriver.
func (d *Driver) Attach(ctx context.Context, container string) (netdriver.Endpoint, error) {
	ep, err := d.s.Attach(ctx, container)
	if err != nil {
		return nil, err
	}
	return endpoint{ep}, nil
}

// Close ends the session.
func (d *Driver) Close() error { return d.s.Close() }

// endpoint adapts *driver.Endpoint (whose Listen and ListenPacket return
// its own types) to netdriver.Endpoint.
type endpoint struct{ *driver.Endpoint }

func (e endpoint) Listen(ctx context.Context, network string, port uint16) (net.Listener, error) {
	l, err := e.Endpoint.Listen(ctx, network, port)
	if err != nil {
		return nil, err
	}
	return l, nil
}

func (e endpoint) ListenPacket(ctx context.Context, network string, port uint16) (net.PacketConn, error) {
	pc, err := e.Endpoint.ListenPacket(ctx, network, port)
	if err != nil {
		return nil, err
	}
	return pc, nil
}

// Join is the student's side of a class: it trades a join code for a
// session token at the relay's API (base, e.g. https://relay:8443), then
// connects the data channel (wss://…/v1/data). The token never appears in
// a URL. hc is the HTTP client (its TLS configuration trusts the relay).
func Join(ctx context.Context, base, code, displayName string, hc *http.Client) (*Joined, error) {
	join := relayv1connect.NewJoinServiceClient(connect.NewClient(connecthttp.NewTransport(hc, strings.TrimSuffix(base, "/"))))
	r, err := join.Join(ctx, &v1.JoinRequest{JoinCode: code, DisplayName: displayName})
	if err != nil {
		return nil, fmt.Errorf("relay: join: %w", err)
	}
	return Connect(ctx, base, r.SessionToken, hc)
}

// DataURL is the data channel's URL for an API base URL.
func DataURL(base string) (string, error) {
	base = strings.TrimSuffix(base, "/")
	switch {
	case strings.HasPrefix(base, "https://"):
		return "wss://" + strings.TrimPrefix(base, "https://") + "/v1/data", nil
	case strings.HasPrefix(base, "http://"):
		return "ws://" + strings.TrimPrefix(base, "http://") + "/v1/data", nil
	}
	return "", fmt.Errorf("relay: %q is not an http(s) URL", base)
}

// Joined is a connected class session that keeps its token fresh.
type Joined struct {
	*Driver
	base string
	hc   *http.Client

	mu    sync.Mutex
	token string
	stop  context.CancelFunc
}

// Connect opens the data channel with a session token and refreshes the
// token before it expires (RefreshSession, then REAUTH: no flow drops).
func Connect(ctx context.Context, base, token string, hc *http.Client) (*Joined, error) {
	u, err := DataURL(base)
	if err != nil {
		return nil, err
	}
	s, err := driver.DialWebSocket(ctx, u, token, hc)
	if err != nil {
		return nil, fmt.Errorf("relay: data channel: %w", err)
	}
	rctx, stop := context.WithCancel(context.Background())
	j := &Joined{Driver: New(s), base: base, hc: hc, token: token, stop: stop}
	go j.refresh(rctx)
	return j, nil
}

// Token is the current session token (to reconnect after a restart).
func (j *Joined) Token() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.token
}

// Close ends the session and its refresher.
func (j *Joined) Close() error {
	j.stop()
	return j.Driver.Close()
}

// refreshMargin is how long before expiry the token is renewed.
const refreshMargin = 10 * time.Minute

func (j *Joined) refresh(ctx context.Context) {
	join := relayv1connect.NewJoinServiceClient(connect.NewClient(connecthttp.NewTransport(j.hc, strings.TrimSuffix(j.base, "/"))))
	exp := time.Unix(j.s.Welcome().GetTokenExpiresUnix(), 0)
	for {
		wait := time.Until(exp) - refreshMargin
		if wait < time.Minute {
			wait = time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-j.s.Done():
			return
		case <-time.After(wait):
		}
		r, err := join.RefreshSession(ctx, &v1.RefreshSessionRequest{SessionToken: j.Token()})
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return
			}
			exp = time.Now().Add(refreshMargin + time.Minute) // retry in a minute
			continue
		}
		if err := j.s.Reauth(r.SessionToken); err != nil {
			return // the session is gone
		}
		j.mu.Lock()
		j.token = r.SessionToken
		j.mu.Unlock()
		exp = time.Unix(r.ExpiresUnix, 0)
	}
}

var _ netdriver.NetworkDriver = (*Driver)(nil)
