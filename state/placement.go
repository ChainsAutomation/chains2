package state

import (
	"errors"
	"slices"

	chains "chains-automation"
)

// PlacementOverride uses nil fields to inherit the service's registered defaults.
type PlacementOverride struct {
	Location *[]string `json:"location,omitempty"`
	Tags     *[]string `json:"tags,omitempty"`
}

type placementWrite struct {
	address  string
	override PlacementOverride
}

var ErrUnknownDevice = errors.New("unknown device")

func (a *Authority) DefaultService(id string) (chains.Service, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.defaults[id]
	return s, ok
}

func (a *Authority) effective(s chains.Service) chains.Service {
	s.Devices = slices.Clone(s.Devices)
	for i := range s.Devices {
		device := &s.Devices[i]
		p := a.placements[Address(s.UUID, device.ID)]
		if p.Location != nil {
			device.Location = *p.Location
		}
		if p.Tags != nil {
			device.Tags = *p.Tags
		}
	}
	return s
}

func (a *Authority) EditPlacement(service, device string, change func(PlacementOverride) (PlacementOverride, error)) (Update, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.defaults[service]
	if !ok || !slices.ContainsFunc(s.Devices, func(d chains.Device) bool { return d.ID == device }) {
		return Update{}, ErrUnknownDevice
	}
	address := Address(service, device)
	old := a.placements[address]
	next, err := change(old)
	if err != nil {
		return Update{}, err
	}
	a.View.mu.RLock()
	epoch, revision := a.data.Epoch, a.data.Revision
	a.View.mu.RUnlock()
	a.placements[address] = next
	effective := a.effective(s)
	u := Update{Epoch: epoch, Revision: revision + 1, Service: &effective, placement: &placementWrite{address, next}}
	applied, err := a.db.commit("placement/"+chains.ID(), u)
	if err != nil || !applied {
		a.placements[address] = old
		return Update{}, err
	}
	a.View.mu.Lock()
	a.apply(u)
	a.View.mu.Unlock()
	return u, nil
}
