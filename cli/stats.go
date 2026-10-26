package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/go-wasmcontainers/engine/api"
)

func blkioWrite(s api.ContainerStats) uint64 {
	for _, e := range s.BlkioStats.IOServiceBytesRecursive {
		if e.Op == "write" {
			return e.Value
		}
	}
	return 0
}

func printStats(w io.Writer, rows []api.ContainerStats) {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "CONTAINER ID\tNAME\tCPU %\tMEM USAGE / LIMIT\tMEM %\tBLOCK I/O\tPIDS")
	for _, s := range rows {
		pct := "0.00%"
		if s.MemoryStats.Limit > 0 {
			pct = fmt.Sprintf("%.2f%%", float64(s.MemoryStats.Usage)*100/float64(s.MemoryStats.Limit))
		}
		fmt.Fprintf(tw, "%s\t%s\t--\t%s / %s\t%s\t0B / %s\t%d\n", shortID(s.ID), strings.TrimPrefix(s.Name, "/"),
			binSize(s.MemoryStats.Usage), binSize(s.MemoryStats.Limit), pct, size(int64(blkioWrite(s))), s.PidsStats.Current)
	}
	tw.Flush()
}

// binSize prints memory in binary units, as docker stats does.
func binSize(n uint64) string {
	const k = 1024
	switch {
	case n >= k*k*k:
		return fmt.Sprintf("%.2fGiB", float64(n)/(k*k*k))
	case n >= k*k:
		return fmt.Sprintf("%.2fMiB", float64(n)/(k*k))
	case n >= k:
		return fmt.Sprintf("%.2fKiB", float64(n)/k)
	}
	return fmt.Sprintf("%dB", n)
}

func statsCmd(client clientFunc) *cobra.Command {
	var noStream, all bool
	cmd := &cobra.Command{
		Use:   "stats [OPTIONS] [CONTAINER...]",
		Short: "Show containers' memory and writes (CPU is not measured: a wasm guest gives no CPU accounting)",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			ids := args
			if len(ids) == 0 {
				list, err := c.Containers(ctx, all)
				if err != nil {
					return err
				}
				for _, l := range list {
					ids = append(ids, l.ID)
				}
			}
			if noStream {
				var rows []api.ContainerStats
				for _, id := range ids {
					err := c.Stats(ctx, id, false, func(s api.ContainerStats) error { rows = append(rows, s); return nil })
					if err != nil {
						return err
					}
				}
				printStats(cmd.OutOrStdout(), rows)
				return nil
			}
			// One stream per container, the table redrawn every second.
			var mu sync.Mutex
			latest := map[string]api.ContainerStats{}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			errs := make(chan error, len(ids))
			for _, id := range ids {
				go func() {
					errs <- c.Stats(ctx, id, true, func(s api.ContainerStats) error {
						mu.Lock()
						latest[id] = s
						mu.Unlock()
						return nil
					})
				}()
			}
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil
				case err := <-errs:
					if err != nil && !errors.Is(err, context.Canceled) {
						return err
					}
				case <-t.C:
					mu.Lock()
					rows := make([]api.ContainerStats, 0, len(ids))
					for _, id := range ids {
						if s, ok := latest[id]; ok {
							rows = append(rows, s)
						}
					}
					mu.Unlock()
					fmt.Fprint(cmd.OutOrStdout(), "\x1b[H\x1b[2J") // clear, as docker stats does
					printStats(cmd.OutOrStdout(), rows)
				}
			}
		},
	}
	cmd.Flags().BoolVar(&noStream, "no-stream", false, "print one reading and exit")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "all containers (default: running ones)")
	return cmd
}
