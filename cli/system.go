package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/docker/go-units"
	"github.com/spf13/cobra"
)

func size(n int64) string { return units.HumanSizeWithPrecision(float64(n), 3) }

func reclaimable(n, total int64) string {
	if total <= 0 {
		return size(n)
	}
	return fmt.Sprintf("%s (%d%%)", size(n), n*100/total)
}

func systemCmd(client clientFunc) *cobra.Command {
	cmd := &cobra.Command{Use: "system", Short: "Manage the engine: disk usage, pruning"}
	cmd.AddCommand(dfCmd(client), pruneCmd(client))
	return cmd
}

func dfCmd(client clientFunc) *cobra.Command {
	var verbose bool
	cmd := &cobra.Command{
		Use:   "df",
		Short: "Show what images, containers and the state store use",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			u, err := c.DiskUsage(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			var activeImgs int
			var unused, rw, rwStopped int64
			for _, i := range u.Images {
				if i.Containers > 0 {
					activeImgs++
				} else {
					unused += i.Size
				}
			}
			var running int
			for _, ct := range u.Containers {
				rw += ct.SizeRw
				if ct.State == "running" {
					running++
				} else {
					rwStopped += ct.SizeRw
				}
			}
			tw := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
			fmt.Fprintln(tw, "TYPE\tTOTAL\tACTIVE\tSIZE\tRECLAIMABLE")
			fmt.Fprintf(tw, "Images\t%d\t%d\t%s\t%s\n", len(u.Images), activeImgs, size(u.LayersSize), reclaimable(unused, u.LayersSize))
			fmt.Fprintf(tw, "Containers\t%d\t%d\t%s\t%s\n", len(u.Containers), running, size(rw), reclaimable(rwStopped, rw))
			fmt.Fprintf(tw, "Local Volumes\t%d\t%d\t%s\t%s\n", len(u.Volumes), 0, size(0), size(0))
			if err := tw.Flush(); err != nil {
				return err
			}
			if verbose {
				fmt.Fprintln(out, "\nImages space usage:")
				tw = tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
				fmt.Fprintln(tw, "REPOSITORY\tTAG\tIMAGE ID\tCREATED\tSIZE\tCONTAINERS")
				for _, i := range u.Images {
					for _, rt := range i.RepoTags {
						repo, tag := splitRepoTag(rt)
						fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\n", repo, tag, shortID(i.ID), ago(time.Unix(i.Created, 0)), size(i.Size), i.Containers)
					}
				}
				tw.Flush()
				fmt.Fprintln(out, "\nContainers space usage:")
				tw = tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
				fmt.Fprintln(tw, "CONTAINER ID\tIMAGE\tCOMMAND\tSIZE\tCREATED\tSTATUS\tNAMES")
				for _, ct := range u.Containers {
					fmt.Fprintf(tw, "%s\t%s\t%q\t%s\t%s\t%s\t%s\n", shortID(ct.ID), ct.Image, truncate(ct.Command, 20), size(ct.SizeRw),
						ago(time.Unix(ct.Created, 0)), ct.Status, strings.TrimPrefix(strings.Join(ct.Names, ","), "/"))
				}
				tw.Flush()
			}
			if s := u.Storage; s != nil {
				fmt.Fprintf(out, "\nState store: %s", s.Kind)
				if s.Usage >= 0 && s.Quota > 0 {
					fmt.Fprintf(out, ", %s used of %s (%d%%)", size(s.Usage), size(s.Quota), s.Usage*100/s.Quota)
				} else if s.Usage >= 0 {
					fmt.Fprintf(out, ", %s used", size(s.Usage))
				}
				if s.Persistent {
					fmt.Fprint(out, ", persistent")
				} else if s.Quota > 0 {
					fmt.Fprint(out, ", best effort (the browser may evict it)")
				}
				fmt.Fprintln(out)
				if s.Error != "" {
					fmt.Fprintln(out, "Last write failed:", s.Error)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show space usage per image and container")
	return cmd
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// confirm asks on in; anything but y or yes (an empty input, no input at
// all) is a no.
func confirm(in io.Reader, out io.Writer, warning string) bool {
	fmt.Fprint(out, warning+"\nAre you sure you want to continue? [y/N] ")
	line, _ := bufio.NewReader(io.LimitReader(in, 256)).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	fmt.Fprintln(out)
	return false
}

func pruneCmd(client clientFunc) *cobra.Command {
	var all, force bool
	cmd := &cobra.Command{
		Use:   "prune [OPTIONS]",
		Short: "Remove stopped containers, and with -a every image no container uses",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			warning := "WARNING! This will remove:\n  - all stopped containers\n  - all dangling images"
			if all {
				warning = "WARNING! This will remove:\n  - all stopped containers\n  - all images without at least one container associated to them"
			}
			if !force && !confirm(cmd.InOrStdin(), out, warning) {
				return &exitError{code: 1, err: errors.New("prune cancelled (use -f to skip the question)")}
			}
			cr, err := c.PruneContainers(cmd.Context())
			if err != nil {
				return err
			}
			if len(cr.ContainersDeleted) > 0 {
				fmt.Fprintln(out, "Deleted Containers:")
				for _, id := range cr.ContainersDeleted {
					fmt.Fprintln(out, id)
				}
				fmt.Fprintln(out)
			}
			ir, err := c.PruneImages(cmd.Context(), all)
			if err != nil {
				return err
			}
			if len(ir.ImagesDeleted) > 0 {
				fmt.Fprintln(out, "Deleted Images:")
				for _, it := range ir.ImagesDeleted {
					if it.Untagged != "" {
						fmt.Fprintln(out, "untagged:", it.Untagged)
					}
					if it.Deleted != "" {
						fmt.Fprintln(out, "deleted:", it.Deleted)
					}
				}
				fmt.Fprintln(out)
			}
			fmt.Fprintln(out, "Total reclaimed space:", size(cr.SpaceReclaimed+ir.SpaceReclaimed))
			return nil
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "remove all unused images, not just dangling ones")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "do not prompt for confirmation")
	return cmd
}
