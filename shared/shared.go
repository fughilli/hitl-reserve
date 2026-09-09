// Package shared defines the framework for host-level SHARED RESOURCES:
// instruments multiplexed across reservable units rather than exclusively held.
//
// The canonical example is a single logic analyzer whose channels tap several
// units' signals: one cheap instrument serves the whole host, and a broker
// serializes access to it and maps each unit to its slice (its channels). A shared
// resource is NOT reserved; any active holder can use it, brokered so concurrent
// requests don't collide on the one device.
//
// A Broker implements one shared resource. Brokers register in a Registry that the
// daemon exposes over a generic endpoint (POST /shared/{name}) and advertises in
// /status. See shared/analyzer for a full implementation.
package shared

import (
	"context"
	"encoding/json"
	"sort"
	"sync"

	"github.com/fughilli/hitl-reserve/api"
)

// Broker owns one shared resource and multiplexes access to it across units.
type Broker interface {
	// Name is the resource's unique id on this host (the {name} in POST /shared/{name}).
	Name() string
	// Kind is the resource class, e.g. "logic-analyzer" (surfaced in Status).
	Kind() string
	// Present reports whether the resource is live (configured AND the hardware is
	// attached). A non-present broker is advertised as present=false and its Access
	// should return a 503-class result.
	Present() bool
	// Describe returns the resource's Status entry (kind, presence, attributes).
	Describe() api.SharedResourceInfo
	// Access brokers one request against the resource on behalf of unit (the unit
	// name, for scoping to that unit's slice; may be ""). body is the raw request
	// payload; it returns the raw response, the HTTP status to send, and an error.
	// Implementations serialize internally so concurrent callers don't collide.
	Access(ctx context.Context, unit string, body []byte) (resp []byte, status int, err error)
}

// Binder is an optional Broker capability: a unit can be BOUND to a slice of the
// resource (e.g. specific analyzer channels) with broker-defined config, and the
// binding contributes capabilities the unit then advertises. The catalog calls
// Bind while resolving units so the placement engine can match on those caps.
type Binder interface {
	// Bind registers unit's slice of this resource from config (raw JSON from the
	// catalog binding) and returns the capabilities the unit gains. It may be called
	// again for the same unit to update the binding (e.g. a runtime re-map).
	Bind(unit string, config json.RawMessage) (caps []string, err error)
}

// Registry holds a host's shared-resource brokers, keyed by name.
type Registry struct {
	mu      sync.RWMutex
	brokers map[string]Broker
	order   []string
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry { return &Registry{brokers: map[string]Broker{}} }

// Register adds a broker (overwriting any with the same name).
func (r *Registry) Register(b Broker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.brokers[b.Name()]; !ok {
		r.order = append(r.order, b.Name())
	}
	r.brokers[b.Name()] = b
}

// Get returns the named broker.
func (r *Registry) Get(name string) (Broker, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.brokers[name]
	return b, ok
}

// List returns all brokers in registration order.
func (r *Registry) List() []Broker {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Broker, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.brokers[n])
	}
	return out
}

// Describe returns the Status entries for every registered resource, sorted by
// name for stable output.
func (r *Registry) Describe() []api.SharedResourceInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]api.SharedResourceInfo, 0, len(r.brokers))
	for _, n := range r.order {
		out = append(out, r.brokers[n].Describe())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
