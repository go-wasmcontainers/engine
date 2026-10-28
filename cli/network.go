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

func networkCmd(client clientFunc) *cobra.Command {
	cmd := &cobra.Command{Use: "network", Short: "Manage networks (run --network NAME; containers reach each other by name)"}

	var labels []string
	var driver string
	create := &cobra.Command{
		Use:   "create [OPTIONS] NETWORK",
		Short: "Create an in-engine network",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			o := api.NetworkCreateRequest{Name: args[0], Driver: driver, Labels: map[string]string{}}
			for _, l := range labels {
				k, v, _ := strings.Cut(l, "=")
				o.Labels[k] = v
			}
			r, err := c.NetworkCreate(cmd.Context(), o)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), r.ID)
			return nil
		},
	}
	create.Flags().StringArrayVar(&labels, "label", nil, "set metadata (KEY=value)")
	create.Flags().StringVarP(&driver, "driver", "d", "bridge", "driver (bridge: an in-engine network)")

	var quiet bool
	ls := &cobra.Command{
		Use:     "ls [OPTIONS]",
		Aliases: []string{"list"},
		Short:   "List networks",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			ns, err := c.Networks(cmd.Context())
			if err != nil {
				return err
			}
			if quiet {
				for _, n := range ns {
					fmt.Fprintln(cmd.OutOrStdout(), shortID(n.ID))
				}
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(tw, "NETWORK ID\tNAME\tDRIVER\tSCOPE\tSUBNET")
			for _, n := range ns {
				subnet := ""
				if len(n.IPAM.Config) > 0 {
					subnet = n.IPAM.Config[0]["Subnet"]
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", shortID(n.ID), n.Name, n.Driver, n.Scope, subnet)
			}
			return tw.Flush()
		},
	}
	ls.Flags().BoolVarP(&quiet, "quiet", "q", false, "only IDs")

	inspect := &cobra.Command{
		Use:   "inspect NETWORK [NETWORK...]",
		Short: "Show networks as JSON, with their running containers' addresses",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var all []json.RawMessage
			for _, n := range args {
				raw, err := c.NetworkInspect(cmd.Context(), n)
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

	rm := &cobra.Command{
		Use:     "rm NETWORK [NETWORK...]",
		Aliases: []string{"remove"},
		Short:   "Remove networks no container uses",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			return each(cmd, args, func(n string) error {
				if err := c.NetworkRemove(cmd.Context(), n); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), n)
				return nil
			})
		},
	}

	var aliases []string
	connect := &cobra.Command{
		Use:   "connect [OPTIONS] NETWORK CONTAINER",
		Short: "Connect a container to a network (a running one at once)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			return c.NetworkConnect(cmd.Context(), args[0], args[1], aliases)
		},
	}
	connect.Flags().StringArrayVar(&aliases, "alias", nil, "add a name the container answers to on this network")

	var force bool
	disconnect := &cobra.Command{
		Use:   "disconnect [OPTIONS] NETWORK CONTAINER",
		Short: "Disconnect a container from a network",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			return c.NetworkDisconnect(cmd.Context(), args[0], args[1], force)
		},
	}
	disconnect.Flags().BoolVarP(&force, "force", "f", false, "accepted for compatibility")

	var yes bool
	var pruneFilters []string
	prune := &cobra.Command{
		Use:   "prune [OPTIONS]",
		Short: "Remove all networks no container uses",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var labels []string
			for _, f := range pruneFilters {
				if rest, ok := strings.CutPrefix(f, "label!="); ok && strings.Contains(rest, "=") {
					k, v, _ := strings.Cut(rest, "=")
					labels = append(labels, k+"!="+v)
				} else if rest, ok := strings.CutPrefix(f, "label="); ok && rest != "" {
					labels = append(labels, rest)
				} else {
					return &exitError{code: 125, err: fmt.Errorf("invalid filter %q (label=KEY[=VALUE] or label!=KEY=VALUE)", f)}
				}
			}
			c, err := client()
			if err != nil {
				return err
			}
			if !yes && !confirm(cmd.InOrStdin(), cmd.OutOrStdout(), "WARNING! This will remove all custom networks not used by at least one container.") {
				return &exitError{code: 1, err: fmt.Errorf("prune cancelled (use -f to skip the question)")}
			}
			r, err := c.NetworkPrune(cmd.Context(), labels)
			if err != nil {
				return err
			}
			if len(r.NetworksDeleted) > 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Deleted Networks:")
				for _, n := range r.NetworksDeleted {
					fmt.Fprintln(cmd.OutOrStdout(), n)
				}
			}
			return nil
		},
	}
	prune.Flags().BoolVarP(&yes, "force", "f", false, "do not ask for confirmation")
	prune.Flags().StringArrayVar(&pruneFilters, "filter", nil, "only networks with these labels (label=KEY[=VALUE], label!=KEY=VALUE)")

	cmd.AddCommand(create, ls, inspect, rm, connect, disconnect, prune)
	return cmd
}
