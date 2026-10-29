package container

import (
	"context"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/internal/testutil"
	"github.com/go-wasmcontainers/engine/runtime"
	"github.com/go-wasmcontainers/engine/runtime/wazero"
)

// gate holds Executor.Start open: the container is Running, its process
// not yet attached — the window a kill used to fall into.
type gate struct {
	runtime.Executor
	entered, release chan struct{}
}

func (g *gate) Start(ctx context.Context, s runtime.Spec) (runtime.Process, error) {
	g.entered <- struct{}{}
	<-g.release
	return g.Executor.Start(ctx, s)
}

func gated(t *testing.T) (*Manager, *gate) {
	e := wazero.New(wazero.Options{CompilationCache: testutil.CompilationCache()})
	t.Cleanup(func() { e.Close(context.Background()) })
	g := &gate{Executor: e, entered: make(chan struct{}), release: make(chan struct{})}
	return NewManager(Options{Executor: g}), g
}

// TestKillBeforeAttach: a kill between "running" and the process being
// attached used to answer success and kill nothing (the spinning guest
// ran on). It must reach the process once it exists.
func TestKillBeforeAttach(t *testing.T) {
	for _, how := range []string{"kill", "rm -f", "shutdown"} {
		t.Run(how, func(t *testing.T) {
			m, g := gated(t)
			c, err := m.Create(CreateOptions{Name: "spin", Lower: image(t), Config: Config{Entrypoint: []string{"probe"}, Cmd: []string{"spin"}}})
			if err != nil {
				t.Fatal(err)
			}
			started := make(chan error, 1)
			go func() { started <- m.Start(context.Background(), c.ID()) }()
			<-g.entered // Running, no process yet
			if s := c.Snapshot().State; s != Running {
				t.Fatalf("state %s inside the window", s)
			}
			done := make(chan error, 1)
			switch how {
			case "kill":
				if err := m.Kill(c.ID()); err != nil {
					t.Fatalf("kill in the window: %v", err)
				}
			case "rm -f":
				go func() { done <- m.Remove(context.Background(), c.ID(), true) }()
			case "shutdown":
				go func() { m.Shutdown(context.Background()); done <- nil }()
			}
			time.Sleep(20 * time.Millisecond) // let rm -f / shutdown act inside the window
			close(g.release)
			if err := <-started; err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := c.Wait(ctx, WaitNotRunning); err != nil {
				t.Fatalf("%s in the window killed nothing: the guest still runs", how)
			}
			if how != "kill" {
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatalf("%s never returned", how)
				}
			}
		})
	}
}
