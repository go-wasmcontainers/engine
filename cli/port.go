package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/portmap"
)

// portFlags puts run's -p, --expose and -P into a create request.
func portFlags(req *api.ContainerCreateRequest, publish, expose []string, all bool) error {
	m, err := portmap.ParseSpecs(publish)
	if err != nil {
		return err
	}
	if len(m) > 0 {
		req.HostConfig.PortBindings = m.API()
	}
	for _, e := range expose {
		// --expose takes a port or a range, never a host part.
		if strings.Contains(e, ":") {
			return fmt.Errorf("%w: invalid --expose %q (a port or a range, without a host part)", portmap.ErrInvalid, e)
		}
		em, err := portmap.ParseSpec(e)
		if err != nil {
			return err
		}
		if req.ExposedPorts == nil {
			req.ExposedPorts = map[string]struct{}{}
		}
		for k := range em {
			req.ExposedPorts[k.String()] = struct{}{}
		}
	}
	req.HostConfig.PublishAllPorts = all
	return nil
}

// summaryPorts reads a summary's Ports (decoded as generic JSON).
func summaryPorts(raw []any) []api.Port {
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var out []api.Port
	json.Unmarshal(b, &out)
	return out
}

// formatPorts is ps's PORTS column, as Docker writes it:
// "127.0.0.1:8080->80/tcp, 53/udp".
func formatPorts(raw []any) string {
	ps := summaryPorts(raw)
	sort.SliceStable(ps, func(i, j int) bool {
		if ps[i].PrivatePort != ps[j].PrivatePort {
			return ps[i].PrivatePort < ps[j].PrivatePort
		}
		return ps[i].Type < ps[j].Type
	})
	parts := make([]string, 0, len(ps))
	for _, p := range ps {
		priv := strconv.Itoa(int(p.PrivatePort)) + "/" + p.Type
		if p.PublicPort == 0 {
			parts = append(parts, priv)
			continue
		}
		parts = append(parts, net.JoinHostPort(p.IP, strconv.Itoa(int(p.PublicPort)))+"->"+priv)
	}
	return strings.Join(parts, ", ")
}

func portCmd(client clientFunc) *cobra.Command {
	return &cobra.Command{
		Use:   "port CONTAINER [PRIVATE_PORT[/PROTO]]",
		Short: "List a container's published ports, or where one private port is published",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			raw, err := c.Inspect(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			var in struct {
				NetworkSettings struct {
					Ports portmap.APIMap `json:"Ports"`
				} `json:"NetworkSettings"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return err
			}
			live, err := portmap.FromAPI(in.NetworkSettings.Ports)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(args) == 2 {
				k, err := portmap.ParseKey(args[1])
				if err != nil {
					return err
				}
				bs := live[k]
				if len(bs) == 0 {
					return fmt.Errorf("no public port '%s' published for %s", k, args[0])
				}
				for _, b := range bs {
					fmt.Fprintln(out, net.JoinHostPort(b.Addr().String(), strconv.Itoa(int(b.HostPort))))
				}
				return nil
			}
			for _, k := range live.Keys() {
				for _, b := range live[k] {
					fmt.Fprintf(out, "%s -> %s\n", k, net.JoinHostPort(b.Addr().String(), strconv.Itoa(int(b.HostPort))))
				}
			}
			return nil
		},
	}
}
