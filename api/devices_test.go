package api

import (
	"slices"
	"testing"
)

func TestDeviceCaps(t *testing.T) {
	for _, c := range []struct {
		reqs []DeviceRequest
		want []string
		ok   bool
	}{
		{nil, nil, true},
		{[]DeviceRequest{GPURequest()}, []string{"gpu"}, true},
		{[]DeviceRequest{GPURequest(), GPURequest()}, []string{"gpu"}, true},
		{[]DeviceRequest{{Capabilities: [][]string{{"tpu"}, {"gpu"}}}}, []string{"gpu"}, true},
		{[]DeviceRequest{{Capabilities: [][]string{{"gpu", "compute"}}}}, nil, false},
		{[]DeviceRequest{{Capabilities: [][]string{{}}}}, nil, false},
		{[]DeviceRequest{{}}, nil, false},
		{[]DeviceRequest{{Driver: "nvidia", Capabilities: [][]string{{"gpu"}}}}, nil, false},
		{[]DeviceRequest{{DeviceIDs: []string{"0"}, Capabilities: [][]string{{"gpu"}}}}, nil, false},
	} {
		got, err := DeviceCaps(c.reqs)
		if (err == nil) != c.ok || !slices.Equal(got, c.want) {
			t.Errorf("DeviceCaps(%+v) = %v, %v; want %v, ok=%v", c.reqs, got, err, c.want, c.ok)
		}
	}
}
