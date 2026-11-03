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

func TestDeviceCapsGPUMemory(t *testing.T) {
	opt := func(v string) DeviceRequest {
		r := GPURequest()
		r.Options = map[string]string{"memory": v}
		return r
	}
	for _, c := range []struct {
		name string
		reqs []DeviceRequest
		want []string
		ok   bool
	}{
		{"grant", []DeviceRequest{GPURequestMemory(1 << 30)}, []string{"gpu", "gpu-memory=1073741824"}, true},
		{"the ceiling", []DeviceRequest{GPURequestMemory(GPUMemoryMax)}, []string{"gpu", "gpu-memory=1610612736"}, true},
		{"above the ceiling", []DeviceRequest{GPURequestMemory(GPUMemoryMax + 1)}, nil, false},
		{"zero", []DeviceRequest{opt("0")}, nil, false},
		{"negative", []DeviceRequest{opt("-4")}, nil, false},
		{"not a number", []DeviceRequest{opt("1g")}, nil, false},
		{"overflow", []DeviceRequest{opt("99999999999999999999")}, nil, false},
		{"unknown option", []DeviceRequest{{Capabilities: [][]string{{"gpu"}}, Options: map[string]string{"foo": "1"}}}, nil, false},
		{"two grants", []DeviceRequest{GPURequestMemory(1 << 20), GPURequestMemory(1 << 21)}, nil, false},
		{"no capability", []DeviceRequest{{Options: map[string]string{"memory": "1048576"}}}, nil, false},
	} {
		got, err := DeviceCaps(c.reqs)
		if (err == nil) != c.ok || !slices.Equal(got, c.want) {
			t.Errorf("%s: DeviceCaps = %v, %v; want %v, ok=%v", c.name, got, err, c.want, c.ok)
		}
	}
	if CheckGPUMemory(GPUMemoryDefault) != nil || CheckGPUMemory(0) == nil {
		t.Error("CheckGPUMemory")
	}
}
