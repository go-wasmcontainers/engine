package daemon

import (
	"strconv"

	"github.com/go-wasmcontainers/engine/container"
	"github.com/go-wasmcontainers/engine/events"
	"github.com/go-wasmcontainers/engine/state"
)

// StorageInfo is where the engine keeps its state and how full it is.
type StorageInfo struct {
	Kind         string
	Usage, Quota int64 // bytes, -1 unknown
	Persistent   bool
	Error        string
}

// DiskUsage is what `system df` reports.
type DiskUsage struct {
	LayersSize int64
	Images     []ImageInfo
	Containers []container.Snapshot
	Storage    StorageInfo
}

// DiskUsage measures images (each once, however many names it has),
// containers' writable layers, and the state store.
func (d *Daemon) DiskUsage() (DiskUsage, error) {
	imgs, err := d.Images()
	if err != nil {
		return DiskUsage{}, err
	}
	u := DiskUsage{Images: imgs, Containers: d.mgr.List(true)}
	for _, i := range imgs {
		u.LayersSize += i.Size
	}
	u.Storage = d.Storage()
	return u, nil
}

// Storage describes the state store: what the embedder reports
// (Options.StorageInfo), plus the last failure to write to it.
func (d *Daemon) Storage() StorageInfo {
	si := StorageInfo{Kind: "none (containers do not survive a restart)", Usage: -1, Quota: -1}
	if s := d.opt.Containers.Store; s != nil {
		si.Kind = "state store"
		if ds, ok := s.(state.Describer); ok {
			si.Kind, si.Usage = ds.Describe()
		}
	}
	if d.opt.StorageInfo != nil {
		si = d.opt.StorageInfo()
	}
	if err := d.mgr.PersistError(); err != nil && si.Error == "" {
		si.Error = err.Error()
	}
	return si
}

// PruneImages removes images no container uses; without all only
// dangling (unnamed) ones, which this store never keeps, so nothing.
func (d *Daemon) PruneImages(all bool) ([]DeleteItem, int64, error) {
	if !all {
		return nil, 0, nil
	}
	imgs, err := d.Images()
	if err != nil {
		return nil, 0, err
	}
	var out []DeleteItem
	var reclaimed int64
	for _, i := range imgs {
		if i.Containers > 0 {
			continue
		}
		items, err := d.RemoveImage(string(i.ID), false)
		out = append(out, items...)
		if err != nil {
			return out, reclaimed, err
		}
		reclaimed += i.Size
	}
	d.events.Publish(events.Event{Type: events.Image, Action: "prune", Attributes: map[string]string{"reclaimed": strconv.FormatInt(reclaimed, 10)}})
	return out, reclaimed, nil
}
