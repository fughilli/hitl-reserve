// Package catalog is the declarative source of truth for what a host offers: the
// resource types it knows, the concrete components wired to it, the reservable
// units those components compose into, and the shared resources multiplexed across
// them. It parses a JSON catalog and resolves it into the []runner.Unit the engine
// places reservations onto plus a populated shared.Registry.
//
// The catalog is intentionally declarative so an operator describes their bench
// once; auto-discovery (package discovery) can layer runtime-detected components on
// top. It is stdlib-only (JSON, not YAML) so the module vendors nothing.
package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/fughilli/hitl-reserve/api"
	"github.com/fughilli/hitl-reserve/runner"
	"github.com/fughilli/hitl-reserve/shared"
)

// Catalog is the on-disk schema.
type Catalog struct {
	Comment string `json:"comment,omitempty"` // free-form note (catalogs have no JSON comments)
	// Host is the host name (defaults to the OS hostname when empty).
	Host string `json:"host,omitempty"`
	// Workspace is a logical grouping (repo/fleet) surfaced in status + metrics so
	// several projects' hosts can share one dashboard.
	Workspace string `json:"workspace,omitempty"`
	// Provisioning, if set, advertises an onboarding network in /status (see
	// api.ProvisioningNetwork) so holders can provision DUTs with no OOB creds.
	Provisioning *api.ProvisioningNetwork `json:"provisioning_network,omitempty"`
	// LeaseSeconds is the heartbeat lease window (default 1800).
	LeaseSeconds int `json:"lease_seconds,omitempty"`
	// SSHPortBase is where auto-assigned unit sshd ports start (default 2222). A
	// unit with an explicit ssh_port keeps it; the rest fill upward, skipping taken.
	SSHPortBase int `json:"ssh_port_base,omitempty"`

	// ResourceTypes are named templates: a kind + baseline capabilities that a
	// component of that type inherits.
	ResourceTypes map[string]ResourceType `json:"resource_types,omitempty"`
	// SharedResources declares the host's multiplexed instruments.
	SharedResources []SharedSpec `json:"shared_resources,omitempty"`
	// Components are the concrete pieces of hardware wired to the host.
	Components []ComponentSpec `json:"components,omitempty"`
	// Units are the reservable units, each composing one or more components.
	Units []UnitSpec `json:"units,omitempty"`
	// Discovery, if present, configures runtime auto-detection of components (see
	// package discovery). Parsed here so the whole config lives in one file.
	Discovery *json.RawMessage `json:"discovery,omitempty"`
}

// ResourceType is a template a component draws its kind + capabilities from.
type ResourceType struct {
	Kind         string   `json:"kind,omitempty"` // "usb" | "network" | ...
	Capabilities []string `json:"capabilities,omitempty"`
}

// ComponentSpec is one concrete piece of hardware.
type ComponentSpec struct {
	Comment      string            `json:"comment,omitempty"`
	Name         string            `json:"name"`
	Type         string            `json:"type"`              // -> ResourceTypes
	Kind         string            `json:"kind,omitempty"`    // overrides the type's kind
	Devices      []string          `json:"devices,omitempty"` // host[:container] device mappings
	Env          map[string]string `json:"env,omitempty"`
	Address      string            `json:"address,omitempty"`      // network reachability
	Capabilities []string          `json:"capabilities,omitempty"` // extras beyond the type
}

// UnitSpec is one reservable unit.
type UnitSpec struct {
	Comment      string              `json:"comment,omitempty"` // free-form note (catalogs have no JSON comments)
	Name         string              `json:"name"`
	Type         string              `json:"type,omitempty"`         // reservation target class
	PinOnly      bool                `json:"pin_only,omitempty"`     // reachable only by explicit name/type target
	Components   []string            `json:"components"`             // component names composing this unit
	SSHPort      int                 `json:"ssh_port,omitempty"`     // 0 = auto-assign from SSHPortBase
	Capabilities []string            `json:"capabilities,omitempty"` // unit-level extras
	Env          map[string]string   `json:"env,omitempty"`          // unit-level env (merged last)
	Shared       []UnitSharedBinding `json:"shared,omitempty"`       // bindings to shared resources
}

// UnitSharedBinding binds a unit to a slice of a shared resource. Config is passed
// to the broker's Bind (broker-defined, e.g. analyzer channels); the capabilities
// it returns are unioned into the unit, plus any explicit Caps here.
type UnitSharedBinding struct {
	Resource string          `json:"resource"`         // shared resource name
	Config   json.RawMessage `json:"config,omitempty"` // broker-defined binding config
	Caps     []string        `json:"caps,omitempty"`   // extra caps this binding contributes
}

// SharedSpec declares a shared resource. Kind selects the broker factory; Config is
// the broker-defined configuration.
type SharedSpec struct {
	Comment string          `json:"comment,omitempty"`
	Name    string          `json:"name"`
	Kind    string          `json:"kind"`
	Config  json.RawMessage `json:"config,omitempty"`
}

// BrokerFactory builds a shared.Broker for a SharedSpec. The daemon supplies a
// factory per kind it supports (keeping this package decoupled from any concrete
// broker implementation).
type BrokerFactory func(name string, config json.RawMessage) (shared.Broker, error)

// Resolved is the outcome of resolving a Catalog.
type Resolved struct {
	Host         string
	Workspace    string
	LeaseSeconds int
	Units        []runner.Unit
	Registry     *shared.Registry
	Discovery    *json.RawMessage
	Provisioning *api.ProvisioningNetwork
}

// Load reads and parses a catalog JSON file.
func Load(path string) (*Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse parses catalog JSON. Unknown fields are rejected so a typo'd key surfaces
// as an error rather than being silently ignored.
func Parse(data []byte) (*Catalog, error) {
	var c Catalog
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse catalog: %w", err)
	}
	return &c, nil
}

// Resolve turns a Catalog into the units + shared registry the daemon runs. It
// validates references (a unit's components exist; a binding's resource exists),
// instantiates each shared resource via its factory (by kind), binds units to
// resources (unioning the resulting capabilities), and auto-assigns sshd ports.
func (c *Catalog) Resolve(factories map[string]BrokerFactory) (*Resolved, error) {
	reg := shared.NewRegistry()

	// Instantiate shared resources.
	sharedByName := map[string]shared.Broker{}
	for _, s := range c.SharedResources {
		if s.Name == "" {
			return nil, fmt.Errorf("shared resource: name is required")
		}
		if _, dup := sharedByName[s.Name]; dup {
			return nil, fmt.Errorf("shared resource %q: duplicate name", s.Name)
		}
		f, ok := factories[s.Kind]
		if !ok {
			return nil, fmt.Errorf("shared resource %q: unknown kind %q (no factory registered)", s.Name, s.Kind)
		}
		b, err := f(s.Name, s.Config)
		if err != nil {
			return nil, fmt.Errorf("shared resource %q: %w", s.Name, err)
		}
		reg.Register(b)
		sharedByName[s.Name] = b
	}

	// Resolve components into runner.Components.
	comps := map[string]runner.Component{}
	for _, cs := range c.Components {
		if cs.Name == "" {
			return nil, fmt.Errorf("component: name is required")
		}
		if _, dup := comps[cs.Name]; dup {
			return nil, fmt.Errorf("component %q: duplicate name", cs.Name)
		}
		rc, err := c.resolveComponent(cs)
		if err != nil {
			return nil, err
		}
		comps[cs.Name] = rc
	}

	// Resolve units.
	base := c.SSHPortBase
	if base == 0 {
		base = 2222
	}
	usedPorts := map[int]bool{}
	for _, u := range c.Units {
		if u.SSHPort != 0 {
			usedPorts[u.SSHPort] = true
		}
	}
	nextPort := base
	allocPort := func() int {
		for usedPorts[nextPort] {
			nextPort++
		}
		usedPorts[nextPort] = true
		return nextPort
	}

	names := map[string]bool{}
	var units []runner.Unit
	for _, us := range c.Units {
		if us.Name == "" {
			return nil, fmt.Errorf("unit: name is required")
		}
		if names[us.Name] {
			return nil, fmt.Errorf("unit %q: duplicate name", us.Name)
		}
		names[us.Name] = true
		if len(us.Components) == 0 {
			return nil, fmt.Errorf("unit %q: at least one component is required", us.Name)
		}

		u := runner.Unit{Name: us.Name, Type: us.Type, PinOnly: us.PinOnly, Env: us.Env}
		capSet := map[string]bool{}
		networkCount := 0
		for _, cn := range us.Components {
			rc, ok := comps[cn]
			if !ok {
				return nil, fmt.Errorf("unit %q: unknown component %q", us.Name, cn)
			}
			u.Components = append(u.Components, rc)
			for _, cap := range rc.Capabilities {
				capSet[cap] = true
			}
			if rc.Kind == "network" {
				networkCount++
			}
		}
		for _, cap := range us.Capabilities {
			capSet[cap] = true
		}

		// Bind shared resources; union the capabilities they grant.
		for _, sb := range us.Shared {
			b, ok := sharedByName[sb.Resource]
			if !ok {
				return nil, fmt.Errorf("unit %q: unknown shared resource %q", us.Name, sb.Resource)
			}
			if binder, ok := b.(shared.Binder); ok {
				caps, err := binder.Bind(us.Name, sb.Config)
				if err != nil {
					return nil, fmt.Errorf("unit %q: bind %q: %w", us.Name, sb.Resource, err)
				}
				for _, cap := range caps {
					capSet[cap] = true
				}
			}
			for _, cap := range sb.Caps {
				capSet[cap] = true
			}
		}

		u.Capabilities = sortedSet(capSet)
		u.Kind = deriveKind(len(u.Components), networkCount)
		if us.SSHPort != 0 {
			u.SSHPort = us.SSHPort
		} else {
			u.SSHPort = allocPort()
		}
		units = append(units, u)
	}

	host := c.Host
	lease := c.LeaseSeconds
	if lease == 0 {
		lease = 1800
	}
	return &Resolved{
		Host: host, Workspace: c.Workspace, LeaseSeconds: lease,
		Units: units, Registry: reg, Discovery: c.Discovery,
		Provisioning: c.Provisioning,
	}, nil
}

// resolveComponent expands a ComponentSpec against its resource type.
func (c *Catalog) resolveComponent(cs ComponentSpec) (runner.Component, error) {
	rc := runner.Component{
		Name:    cs.Name,
		Type:    cs.Type,
		Kind:    cs.Kind,
		Devices: cs.Devices,
		Env:     cs.Env,
		Address: cs.Address,
	}
	capSet := map[string]bool{}
	if cs.Type != "" {
		rt, ok := c.ResourceTypes[cs.Type]
		if !ok {
			return runner.Component{}, fmt.Errorf("component %q: unknown resource type %q", cs.Name, cs.Type)
		}
		if rc.Kind == "" {
			rc.Kind = rt.Kind
		}
		for _, cap := range rt.Capabilities {
			capSet[cap] = true
		}
	}
	for _, cap := range cs.Capabilities {
		capSet[cap] = true
	}
	rc.Capabilities = sortedSet(capSet)
	return rc, nil
}

// deriveKind gives a unit a coarse descriptor from its component makeup.
func deriveKind(total, network int) string {
	switch {
	case total > 1:
		return "composite"
	case network == total && total > 0:
		return "network"
	default:
		return "usb"
	}
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
