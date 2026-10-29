//go:build unix

package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// sizeChanges calls fn on each SIGWINCH until ctx ends.
func sizeChanges(ctx context.Context, fn func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				fn()
			}
		}
	}()
}
