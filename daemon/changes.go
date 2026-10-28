package daemon

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/go-wasmcontainers/engine/container"
	"github.com/go-wasmcontainers/engine/image"
)

// MaxChanges bounds the instructions of one commit.
const MaxChanges = 64

// ApplyChanges applies `commit --change` instructions (Dockerfile syntax,
// one per entry) to c: CMD, ENTRYPOINT, ENV, EXPOSE, LABEL, ONBUILD,
// STOPSIGNAL, USER, VOLUME, WORKDIR, as Docker's commit allows. CMD and
// ENTRYPOINT take the JSON (exec) form or the shell form, which becomes
// ["/bin/sh", "-c", "…"] as in a Dockerfile.
func ApplyChanges(c image.ExecConfig, changes []string) (image.ExecConfig, error) {
	if len(changes) > MaxChanges {
		return c, fmt.Errorf("%w: at most %d changes", container.ErrInvalid, MaxChanges)
	}
	bad := func(format string, a ...any) (image.ExecConfig, error) {
		return c, fmt.Errorf("%w: "+format, append([]any{container.ErrInvalid}, a...)...)
	}
	for _, ch := range changes {
		ch = strings.TrimSpace(ch)
		if len(ch) > 4096 {
			return bad("a change is longer than 4096 bytes")
		}
		instr, arg, _ := strings.Cut(ch, " ")
		arg = strings.TrimSpace(arg)
		switch strings.ToUpper(instr) {
		case "CMD", "ENTRYPOINT":
			v, err := execForm(arg)
			if err != nil {
				return bad("%s: %v", instr, err)
			}
			if strings.EqualFold(instr, "CMD") {
				c.Cmd = v
			} else {
				c.Entrypoint = v
			}
		case "ENV":
			kvs, err := pairs(arg, true)
			if err != nil {
				return bad("ENV: %v", err)
			}
			for _, kv := range kvs {
				c.Env = mergeEnv(c.Env, []string{kv[0] + "=" + kv[1]})
			}
		case "LABEL":
			kvs, err := pairs(arg, false)
			if err != nil {
				return bad("LABEL: %v", err)
			}
			labels := map[string]string{}
			for k, v := range c.Labels {
				labels[k] = v
			}
			for _, kv := range kvs {
				labels[kv[0]] = kv[1]
			}
			c.Labels = labels
		case "WORKDIR":
			if arg == "" {
				return bad("WORKDIR needs a path")
			}
			c.WorkingDir = arg
		case "USER":
			c.User = arg
		case "STOPSIGNAL":
			c.StopSignal = arg
		case "EXPOSE":
			ports := map[string]struct{}{}
			for k := range c.ExposedPorts {
				ports[k] = struct{}{}
			}
			for _, p := range strings.Fields(arg) {
				if !strings.Contains(p, "/") {
					p += "/tcp"
				}
				ports[p] = struct{}{}
			}
			c.ExposedPorts = ports
		case "VOLUME":
			vols := map[string]struct{}{}
			for k := range c.Volumes {
				vols[k] = struct{}{}
			}
			list := strings.Fields(arg)
			if strings.HasPrefix(arg, "[") {
				if err := json.Unmarshal([]byte(arg), &list); err != nil {
					return bad("VOLUME: %v", err)
				}
			}
			for _, v := range list {
				vols[v] = struct{}{}
			}
			c.Volumes = vols
		case "ONBUILD":
			c.OnBuild = append(append([]string{}, c.OnBuild...), arg)
		default:
			return bad("%q is not an instruction commit can change", instr)
		}
	}
	return c, nil
}

// execForm reads ["a","b"] or a shell-form command line.
func execForm(arg string) ([]string, error) {
	if strings.HasPrefix(arg, "[") {
		var v []string
		if err := json.Unmarshal([]byte(arg), &v); err != nil {
			return nil, err
		}
		return v, nil
	}
	if arg == "" {
		return nil, fmt.Errorf("no command")
	}
	return []string{"/bin/sh", "-c", arg}, nil
}

// pairs reads "k=v k2=\"v 2\"" or, when legacy, the old "k v" form.
func pairs(arg string, legacy bool) ([][2]string, error) {
	if arg == "" {
		return nil, fmt.Errorf("nothing to set")
	}
	if !strings.Contains(strings.Fields(arg)[0], "=") {
		if !legacy {
			return nil, fmt.Errorf("want key=value")
		}
		k, v, _ := strings.Cut(arg, " ")
		return [][2]string{{k, strings.TrimSpace(v)}}, nil
	}
	var out [][2]string
	for rest := arg; strings.TrimSpace(rest) != ""; {
		rest = strings.TrimLeft(rest, " \t")
		k, after, ok := strings.Cut(rest, "=")
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			return nil, fmt.Errorf("want key=value at %q", rest)
		}
		var v string
		if strings.HasPrefix(after, "\"") {
			end := strings.IndexByte(after[1:], '"')
			if end < 0 {
				return nil, fmt.Errorf("unterminated quote")
			}
			v, rest = after[1:1+end], after[2+end:]
		} else {
			v, rest, _ = strings.Cut(after, " ")
		}
		out = append(out, [2]string{k, v})
	}
	return out, nil
}
