package image

import (
	"fmt"
	"strings"
)

// ErrNoPlatform is returned when an index has no manifest the engine runs.
var ErrNoPlatform = fmt.Errorf("no matching manifest for %s in the index", WasmPlatform)

// SelectManifest picks the wasip1/wasm image manifest of an index. Entries
// that are not image manifests (attestations, nested indexes, unknown
// types) are skipped, as image-index.md requires of unknown media types.
func SelectManifest(x *Index, want Platform) (Descriptor, error) {
	var seen []string
	for _, d := range x.Manifests {
		if !IsManifestType(d.MediaType) {
			continue
		}
		if d.Platform == nil {
			seen = append(seen, "(no platform)")
			continue
		}
		if d.Platform.OS == want.OS && d.Platform.Architecture == want.Architecture {
			return d, nil
		}
		seen = append(seen, d.Platform.String())
	}
	if len(seen) == 0 {
		return Descriptor{}, fmt.Errorf("%w (index holds no image manifest)", ErrNoPlatform)
	}
	return Descriptor{}, fmt.Errorf("%w (index holds %s)", ErrNoPlatform, strings.Join(seen, ", "))
}

// CheckPlatform refuses a config that is not for wasip1/wasm.
func CheckPlatform(c *Config) error {
	if c.OS != WasmPlatform.OS || c.Architecture != WasmPlatform.Architecture {
		return fmt.Errorf("image platform %s/%s is not %s", c.OS, c.Architecture, WasmPlatform)
	}
	return nil
}
