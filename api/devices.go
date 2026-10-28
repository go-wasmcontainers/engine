package api

import (
	"fmt"
	"slices"
	"strings"
)

// KnownCaps are the device capabilities this engine grants: "gpu" opens
// the local GPU to a container's host modules (wasi:webgpu, wasi-nn on the
// GPU).
var KnownCaps = []string{"gpu"}

// GPURequest is what run --gpu sends: every device with the capability
// "gpu" (Docker's --gpus all).
func GPURequest() DeviceRequest {
	return DeviceRequest{Count: -1, Capabilities: [][]string{{"gpu"}}}
}

// DeviceCaps turns device requests into the capabilities they grant. A
// request is satisfied by the first of its OR alternatives made only of
// known capabilities; a request none of whose alternatives is known, or
// one naming a driver or device IDs (there are no host devices to pick in
// a WebAssembly engine), is refused.
func DeviceCaps(reqs []DeviceRequest) ([]string, error) {
	var caps []string
	for _, r := range reqs {
		if r.Driver != "" || len(r.DeviceIDs) > 0 {
			return nil, fmt.Errorf("device request: drivers and device IDs are not supported (use the capability: gpu)")
		}
		var got []string
		for _, and := range r.Capabilities {
			if len(and) > 0 && !slices.ContainsFunc(and, func(c string) bool { return !slices.Contains(KnownCaps, c) }) {
				got = and
				break
			}
		}
		if got == nil {
			return nil, fmt.Errorf("device request: no supported capability in %v (supported: %s)", r.Capabilities, strings.Join(KnownCaps, ", "))
		}
		for _, c := range got {
			if !slices.Contains(caps, c) {
				caps = append(caps, c)
			}
		}
	}
	return caps, nil
}
