//go:build !unix

package cli

import (
	"context"
	"time"
)

// sizeChanges calls fn twice a second until ctx ends: there is no
// SIGWINCH here (Windows), so the size is sampled; fn sends only changes.
func sizeChanges(ctx context.Context, fn func()) {
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				fn()
			}
		}
	}()
}
