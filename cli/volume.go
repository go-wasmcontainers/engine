package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/go-wasmcontainers/engine/api"
)

func volumeCmd(client clientFunc) *cobra.Command {
	cmd := &cobra.Command{Use: "volume", Short: "Manage named volumes (run -v NAME:/path[:ro])"}

	var labels []string
	create := &cobra.Command{
		Use:   "create [OPTIONS] [VOLUME]",
		Short: "Create a volume (a name is generated when none is given)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			o := api.VolumeCreateOptions{Labels: map[string]string{}}
			if len(args) == 1 {
				o.Name = args[0]
			}
			for _, l := range labels {
				k, v, _ := strings.Cut(l, "=")
				o.Labels[k] = v
			}
			v, err := c.VolumeCreate(cmd.Context(), o)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), v.Name)
			return nil
		},
	}
	create.Flags().StringArrayVar(&labels, "label", nil, "set metadata (KEY=value)")

	var quiet bool
	ls := &cobra.Command{
		Use:     "ls [OPTIONS]",
		Aliases: []string{"list"},
		Short:   "List volumes",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			vs, err := c.Volumes(cmd.Context())
			if err != nil {
				return err
			}
			if quiet {
				for _, v := range vs {
					fmt.Fprintln(cmd.OutOrStdout(), v.Name)
				}
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(tw, "DRIVER\tVOLUME NAME")
			for _, v := range vs {
				fmt.Fprintf(tw, "%s\t%s\n", v.Driver, v.Name)
			}
			return tw.Flush()
		},
	}
	ls.Flags().BoolVarP(&quiet, "quiet", "q", false, "only names")

	inspect := &cobra.Command{
		Use:   "inspect VOLUME [VOLUME...]",
		Short: "Show volumes as JSON",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var all []json.RawMessage
			for _, n := range args {
				raw, err := c.VolumeInspect(cmd.Context(), n)
				if err != nil {
					return err
				}
				all = append(all, raw)
			}
			b, _ := json.Marshal(all)
			var out bytes.Buffer
			json.Indent(&out, b, "", "    ")
			fmt.Fprintln(cmd.OutOrStdout(), out.String())
			return nil
		},
	}

	var force bool
	rm := &cobra.Command{
		Use:     "rm [OPTIONS] VOLUME [VOLUME...]",
		Aliases: []string{"remove"},
		Short:   "Remove volumes no container uses",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			return each(cmd, args, func(n string) error {
				if err := c.VolumeRemove(cmd.Context(), n, force); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), n)
				return nil
			})
		},
	}
	rm.Flags().BoolVarP(&force, "force", "f", false, "no error when a volume does not exist")

	var all, yes bool
	prune := &cobra.Command{
		Use:   "prune [OPTIONS]",
		Short: "Remove unused anonymous volumes (-a: every unused volume)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			warning := "WARNING! This will remove anonymous local volumes not used by at least one container."
			if all {
				warning = "WARNING! This will remove all local volumes not used by at least one container."
			}
			if !yes && !confirm(cmd.InOrStdin(), cmd.OutOrStdout(), warning) {
				return &exitError{code: 1, err: fmt.Errorf("prune cancelled (use -f to skip the question)")}
			}
			r, err := c.VolumesPrune(cmd.Context(), all)
			if err != nil {
				return err
			}
			if len(r.VolumesDeleted) > 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Deleted Volumes:")
				for _, n := range r.VolumesDeleted {
					fmt.Fprintln(cmd.OutOrStdout(), n)
				}
				fmt.Fprintln(cmd.OutOrStdout())
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Total reclaimed space:", size(r.SpaceReclaimed))
			return nil
		},
	}
	prune.Flags().BoolVarP(&all, "all", "a", false, "remove all unused volumes, not just anonymous ones")
	prune.Flags().BoolVarP(&yes, "force", "f", false, "do not prompt for confirmation")

	cmd.AddCommand(create, ls, inspect, rm, prune)
	return cmd
}
