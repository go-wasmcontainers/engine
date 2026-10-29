package portmap

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// APIBinding is Docker's PortBinding: strings, an empty HostIp for the
// default address, an empty HostPort for one the system picks.
type APIBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// API is the binding as the Engine API writes it.
func (b Binding) API() APIBinding {
	out := APIBinding{}
	if b.HostIP.IsValid() {
		out.HostIP = b.HostIP.String()
	}
	if b.HostPort != 0 {
		out.HostPort = strconv.Itoa(int(b.HostPort))
		if b.HostPortEnd > b.HostPort {
			out.HostPort += "-" + strconv.Itoa(int(b.HostPortEnd))
		}
	}
	return out
}

// APIMap is Docker's PortMap: "80/tcp" -> bindings, null for a port only
// exposed.
type APIMap map[string][]APIBinding

// API is the map as the Engine API writes it.
func (m Map) API() APIMap {
	if m == nil {
		return nil
	}
	out := APIMap{}
	for k, bs := range m {
		var list []APIBinding
		for _, b := range bs {
			list = append(list, b.API())
		}
		out[k.String()] = list
	}
	return out
}

// FromAPI reads a PortMap from the API or from storage.
func FromAPI(a APIMap) (Map, error) {
	if a == nil {
		return nil, nil
	}
	m := Map{}
	for ks, bs := range a {
		k, err := ParseKey(ks)
		if err != nil {
			return nil, err
		}
		if _, ok := m[k]; !ok {
			m[k] = nil
		}
		for _, ab := range bs {
			b, err := ParseBinding(ab.HostIP, ab.HostPort)
			if err != nil {
				return nil, err
			}
			m[k] = append(m[k], b)
		}
		if m.Bindings() > MaxBindings {
			return nil, fmt.Errorf("%w: at most %d published ports", ErrInvalid, MaxBindings)
		}
	}
	return m, nil
}

// MarshalJSON writes the Engine API's shape.
func (m Map) MarshalJSON() ([]byte, error) { return json.Marshal(m.API()) }

// UnmarshalJSON reads it, validating every key and binding.
func (m *Map) UnmarshalJSON(b []byte) error {
	var a APIMap
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	got, err := FromAPI(a)
	if err != nil {
		return err
	}
	*m = got
	return nil
}

// Exposed adds ports to m as exposed (no binding), keeping bindings it
// already has: Docker's Config.ExposedPorts and the image's.
func (m Map) Exposed(keys map[string]struct{}) error {
	for ks := range keys {
		k, err := ParseKey(ks)
		if err != nil {
			return err
		}
		if _, ok := m[k]; !ok {
			m[k] = nil
		}
	}
	return nil
}

// PublishAll is m with every port that has no binding given one the system
// picks: run -P, HostConfig.PublishAllPorts.
func (m Map) PublishAll() Map {
	out := Map{}
	for k, bs := range m {
		if len(bs) == 0 {
			bs = []Binding{{}}
		}
		out[k] = append([]Binding(nil), bs...)
	}
	return out
}
