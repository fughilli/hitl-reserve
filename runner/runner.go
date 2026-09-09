// Package runner brings a reservation's environment up and tears it down. The
// Runner interface is the pluggable execution backend; PodmanRunner (podman.go)
// is the reference implementation, shelling out to `podman` to launch an
// SSH-reachable container with the unit's components attached.
package runner

import (
	"context"

	"github.com/fughilli/hitl-reserve/api"
)

// Component is one atomic piece of hardware that makes up a unit: a USB board, a
// network host, an SDR, etc. A unit's components are attached together into the
// same environment when the unit activates, which is how a composite unit (e.g.
// an ESP32-C6 + a HackRF) is handed out as a single reservation.
type Component struct {
	// Name is the component's stable identifier, unique within its unit.
	Name string
	// Type names the component's resource type (an entry in the catalog's
	// ResourceTypes), from which its baseline Capabilities are drawn.
	Type string
	// Kind selects how the component is wired into the environment:
	//   - "" or "usb": a board attached over USB; its Devices are mapped in and,
	//     where possible, isolated to just this board's raw-USB node.
	//   - "network": a device reached over the LAN with no local board; it gets no
	//     USB mounts, and its Address is injected as env for the environment to dial.
	// Other kinds are treated like "usb" for wiring (explicit Devices, no isolation
	// guarantees) — a backend may special-case them.
	Kind string
	// Capabilities are the component's features (from its resource type plus any
	// per-component extras). Unioned into the unit's advertised capabilities.
	Capabilities []string
	// Devices are `--device`-style mappings, each "host" or "host:container"
	// (e.g. "/dev/serial/by-id/…:/dev/ttyACM0" pins a board's tty to a stable
	// in-container path). Resolved (through by-id symlinks) at Start.
	Devices []string
	// Env are environment variables set in the environment for this component
	// (e.g. an adapter serial so a debugger targets this exact board).
	Env map[string]string
	// Address is a network component's reachable address (host or ip[:port]);
	// injected into the environment (see PodmanConfig.AddressEnv) so the holder can
	// dial it. Empty for USB components.
	Address string
}

// Unit is a reservable unit: the atomic thing a client reserves. It bundles one
// or more components plus the metadata the engine uses to place reservations.
type Unit struct {
	// Name is the stable unit identifier; matches ReserveRequest.Unit.
	Name string
	// Type is the unit type (matches ReserveRequest.UnitType) — a class of
	// interchangeable units, e.g. "esp32c6" or "esp32c6+hackrf".
	Type string
	// Kind is a coarse descriptor derived from the components: "network" when every
	// component is network, "composite" for more than one component, else "usb".
	// Informational; placement uses PinOnly/Capabilities, not Kind.
	Kind string
	// PinOnly makes the unit reachable ONLY by an explicit target (a Unit name or a
	// UnitType), never by a bare "any unit" or capability-only request — so scarce
	// or fragile units (e.g. the one network player) don't absorb ordinary work.
	PinOnly bool
	// Capabilities is the unit's advertised capability set: the union of its
	// components' capabilities, any unit-level extras, and the capabilities
	// contributed by bound shared resources. A reservation matches when its
	// RequireCaps are a subset of this.
	Capabilities []string
	// Components are the pieces of hardware attached together for the reservation.
	Components []Component
	// SSHPort is the host port published to this unit's environment sshd. Distinct
	// per unit so several units' environments coexist.
	SSHPort int
	// Env are extra environment variables set for the whole unit (merged after each
	// component's Env; unit-level keys win on conflict).
	Env map[string]string
}

// Runner brings a reservation's environment up and tears it down. Implementations
// must be safe to Stop a Start that never completed, and Stop for an unknown id.
type Runner interface {
	// Start launches the environment for reservation id on the given unit,
	// authorizing sshKey, and returns the endpoint to reach it. It should be
	// idempotent-ish: a second Start for the same id (after a crash) recovers or
	// replaces cleanly.
	Start(ctx context.Context, id, owner, sshKey string, unit Unit) (*api.Endpoint, error)
	// Stop tears the environment down. Safe for an unknown/already-gone id.
	Stop(ctx context.Context, id string) error
	// Cleanup removes any leftover environments (e.g. at daemon startup after a
	// crash) so a fresh queue starts from a clean slate.
	Cleanup(ctx context.Context) error
}
