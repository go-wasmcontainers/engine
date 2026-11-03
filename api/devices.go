package api

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// KnownCaps are the device capabilities this engine grants: "gpu" opens
// the local GPU to a container's host modules (wasi:webgpu, wasi-nn on the
// GPU, wasi_webgpu_batch).
var KnownCaps = []string{"gpu"}

// GPU memory grant of a container's webgpu-batch contexts (run
// --gpu-memory). The request carries it as Options["memory"], the shape of
// Docker's DeviceRequest.Options; the grant reaches the container's host
// modules as the capability "gpu-memory=BYTES" (wasi.Proc.CapValue).
const (
	// GPUMemoryOption is the DeviceRequest.Options key.
	GPUMemoryOption = "memory"
	// GPUMemoryCap is the name of the capability that carries the grant.
	GPUMemoryCap = "gpu-memory"
	// GPUMemoryDefault applies when no grant was made: the 256 MiB quota
	// every GPU host module has.
	GPUMemoryDefault int64 = 256 << 20
	// GPUMemoryMax is the largest grant, 1.5 GiB.
	GPUMemoryMax int64 = 1610612736
)

// GPURequest is what run --gpu sends: every device with the capability
// "gpu" (Docker's --gpus all).
func GPURequest() DeviceRequest {
	return DeviceRequest{Count: -1, Capabilities: [][]string{{"gpu"}}}
}

// GPURequestMemory is GPURequest with a GPU memory grant of n bytes. n is
// checked by DeviceCaps (and by the client before it sends the request).
func GPURequestMemory(n int64) DeviceRequest {
	r := GPURequest()
	r.Options = map[string]string{GPUMemoryOption: strconv.FormatInt(n, 10)}
	return r
}

// CheckGPUMemory refuses a GPU memory grant that is not positive or is
// above GPUMemoryMax.
func CheckGPUMemory(n int64) error {
	switch {
	case n <= 0:
		return fmt.Errorf("GPU memory must be positive, got %d", n)
	case n > GPUMemoryMax:
		return fmt.Errorf("GPU memory %d bytes is above the %d-byte (1.5 GiB) ceiling", n, GPUMemoryMax)
	}
	return nil
}

// DeviceCaps turns device requests into the capabilities they grant. A
// request is satisfied by the first of its OR alternatives made only of
// known capabilities; a request none of whose alternatives is known, or
// one naming a driver or device IDs (there are no host devices to pick in
// a WebAssembly engine), is refused. Options["memory"] is a GPU memory
// grant in bytes: it needs the capability "gpu" in the same request, is at
// most GPUMemoryMax, and is returned as the capability "gpu-memory=BYTES";
// any other option is refused.
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
		for k, v := range r.Options {
			if k != GPUMemoryOption {
				return nil, fmt.Errorf("device request: unknown option %q (supported: %s)", k, GPUMemoryOption)
			}
			if !slices.Contains(got, "gpu") {
				return nil, fmt.Errorf("device request: option %q needs the capability gpu", k)
			}
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("device request: option %q is not a number of bytes: %q", k, v)
			}
			if err := CheckGPUMemory(n); err != nil {
				return nil, fmt.Errorf("device request: %w", err)
			}
			for _, c := range caps {
				if strings.HasPrefix(c, GPUMemoryCap+"=") {
					return nil, fmt.Errorf("device request: two GPU memory grants")
				}
			}
			caps = append(caps, GPUMemoryCap+"="+strconv.FormatInt(n, 10))
		}
	}
	return caps, nil
}
