package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/go-wasmcontainers/engine/api"
)

// formatEvent prints an event as docker events does:
// "<time> <type> <action> <id> (k=v, …)", attributes sorted.
func formatEvent(w io.Writer, m api.EventMessage) {
	keys := make([]string, 0, len(m.Actor.Attributes))
	for k := range m.Actor.Attributes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	attrs := make([]string, len(keys))
	for i, k := range keys {
		attrs[i] = k + "=" + m.Actor.Attributes[k]
	}
	line := time.Unix(0, m.TimeNano).Format(time.RFC3339Nano) + " " + m.Type + " " + m.Action
	if m.Actor.ID != "" {
		line += " " + m.Actor.ID
	}
	if len(attrs) > 0 {
		line += " (" + strings.Join(attrs, ", ") + ")"
	}
	fmt.Fprintln(w, line)
}

func eventsCmd(client clientFunc) *cobra.Command {
	var since, until string
	var filters []string
	cmd := &cobra.Command{
		Use:   "events [OPTIONS]",
		Short: "Stream what happens to containers, images and volumes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			o := api.EventsOptions{Since: since, Until: until, Filters: map[string][]string{}}
			for _, f := range filters {
				k, v, ok := strings.Cut(f, "=")
				if !ok || k == "" {
					return &exitError{code: 125, err: fmt.Errorf("bad format of filter (expected name=value): %q", f)}
				}
				o.Filters[k] = append(o.Filters[k], v)
			}
			return c.Events(cmd.Context(), o, func(m api.EventMessage) error {
				formatEvent(cmd.OutOrStdout(), m)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "show events since this time (Unix seconds, RFC 3339, or a duration such as 10m)")
	cmd.Flags().StringVar(&until, "until", "", "stop at this time (same forms)")
	cmd.Flags().StringArrayVarP(&filters, "filter", "f", nil, "filter: type=, event=, container=, image=, volume=, label=")
	return cmd
}
