// Package api defines the wire types shared by the reservation daemon
// (hitl-reserved) and the client CLI (hitl). It is deliberately dependency-free
// (stdlib only) so any project can import it to talk to a reservation host.
//
// # Model
//
// A reservation HOST (one machine running hitl-reserved) offers one or more
// reservable UNITS. A unit is the atomic thing a client reserves; it is composed
// of one or more COMPONENTS — individual pieces of hardware wired to the host:
//
//   - a single USB-attached board (e.g. an ESP32-C6 dev board) is a unit with one
//     USB component;
//   - a network-attached device (e.g. a Raspberry Pi player reached over the LAN)
//     is a unit with one network component;
//   - a bundle of physically-connected hardware (e.g. an ESP32-C6 wired to a
//     HackRF One SDR) is a unit with several components, reserved together.
//
// Each unit advertises CAPABILITIES (the union of its components' capabilities
// plus any it gains from bound shared resources). Clients target a unit three
// ways: by name (pin to one exact unit), by unit type (any free unit of a type),
// or by required capabilities (any free unit whose capabilities are a superset).
//
// SHARED RESOURCES are host-level instruments multiplexed across units rather
// than exclusively held — e.g. one logic analyzer whose channels tap several
// units' signals, brokered so concurrent reservations serialize on the single
// device. A unit binds slices of a shared resource; the daemon advertises the
// binding as capabilities and brokers access over a generic endpoint.
package api

import "time"

// State is the lifecycle of a reservation.
type State string

const (
	// StateQueued: waiting behind others; not yet allocated a unit.
	StateQueued State = "queued"
	// StateActive: holds a unit; the runner has brought its environment up.
	StateActive State = "active"
	// StateReleased: released by the holder or expired; terminal.
	StateReleased State = "released"
)

// ReserveRequest enqueues a new reservation. The three targeting fields are
// checked most-specific first: Unit (an exact unit), then UnitType (any unit of a
// type), then RequireCaps (any unit whose capabilities are a superset). They
// combine: e.g. UnitType+RequireCaps means "a unit of this type that also has
// these capabilities".
type ReserveRequest struct {
	// Owner is a free-form identifier for logs/status (e.g. an agent or issue id).
	Owner string `json:"owner"`
	// SSHPublicKey is the OpenSSH public key authorized on the unit's environment
	// while this reservation is active (e.g. "ssh-ed25519 AAAA… agent").
	SSHPublicKey string `json:"ssh_public_key"`
	// Unit pins the reservation to a specific unit by name (see UnitStatus.Name).
	// Empty means "any matching unit". An explicit name may target a pin-only unit.
	Unit string `json:"unit,omitempty"`
	// UnitType pins the reservation to any free unit of this type (see
	// UnitStatus.Type). Like Unit (and unlike RequireCaps) it is an explicit
	// hardware target, so it may land on a pin-only unit.
	UnitType string `json:"unit_type,omitempty"`
	// RequireCaps restricts the reservation to a unit whose advertised Capabilities
	// are a superset of these. On its own it means "any free unit with these
	// capabilities"; it selects among best-fitting units (fewest extra
	// capabilities) but never a pin-only unit — reaching one needs Unit or UnitType.
	RequireCaps []string `json:"require_caps,omitempty"`
}

// Endpoint is where the holder connects once active. The reservation flow is
// transport-agnostic: the client reads Host/Port/User out of the response and
// never assumes a fixed port, so distinct units publishing distinct ports need no
// client changes.
type Endpoint struct {
	Host string `json:"host"` // reach the host (e.g. its tailnet name)
	Port int    `json:"port"` // published sshd port for this unit's environment
	User string `json:"user"` // login user inside the environment
}

// Reservation is the full server-side view of one reservation.
type Reservation struct {
	ID        string     `json:"id"`
	Owner     string     `json:"owner"`
	State     State      `json:"state"`
	Position  int        `json:"position"` // 0 == active/head, else waiters ahead
	CreatedAt time.Time  `json:"created_at"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"` // lease deadline
	Endpoint  *Endpoint  `json:"endpoint,omitempty"`   // set once active
	// Unit is the unit this reservation landed on (set once active).
	Unit string `json:"unit,omitempty"`
	// UnitType mirrors the landed unit's type (informational).
	UnitType string `json:"unit_type,omitempty"`
	// Message carries human-readable context (e.g. why released).
	Message string `json:"message,omitempty"`
}

// ComponentStatus is one component's slice of a unit's status.
type ComponentStatus struct {
	Name         string   `json:"name"`
	Type         string   `json:"type,omitempty"`
	Kind         string   `json:"kind,omitempty"`         // "usb" | "network" | ...
	Capabilities []string `json:"capabilities,omitempty"` // from the resource type + extras
}

// UnitStatus is one reservable unit's slice of a host's Status.
type UnitStatus struct {
	Name         string            `json:"name"`
	Type         string            `json:"type,omitempty"`
	Kind         string            `json:"kind,omitempty"`         // "usb" | "network" | "composite"
	PinOnly      bool              `json:"pin_only,omitempty"`     // reachable only by an explicit Unit/UnitType target
	Capabilities []string          `json:"capabilities,omitempty"` // union (components + shared-resource bindings)
	Components   []ComponentStatus `json:"components,omitempty"`
	Active       *Reservation      `json:"active"` // this unit's holder, or null if free
}

// SharedResourceInfo advertises a host-level shared resource (an instrument
// multiplexed across units, not exclusively reserved) so clients can select a
// host by the resources it offers. Attributes is broker-defined free-form JSON
// (e.g. a logic analyzer reports its driver, protocols, and mapped channels).
type SharedResourceInfo struct {
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`    // e.g. "logic-analyzer"
	Present    bool           `json:"present"` // true when the instrument is live
	Attributes map[string]any `json:"attributes,omitempty"`
}

// ProvisioningNetwork advertises a network a client can onboard a device-under-test
// onto without any out-of-band credentials — e.g. a host that runs a WiFi access
// point which DUTs are provisioned onto (over BLE/Improv or similar) so the host
// can then reach them. Advertised in Status; nil when the host runs none. The
// credential is returned deliberately: the reservation transport (a trusted
// network) is the security boundary, so a holder needs the PSK to drive
// provisioning, the same posture as the reservation SSH key.
type ProvisioningNetwork struct {
	SSID string `json:"ssid"`
	PSK  string `json:"psk,omitempty"`
}

// Status is the daemon's overall view of a host.
type Status struct {
	Host         string               `json:"host"`                   // host name
	Workspace    string               `json:"workspace,omitempty"`    // logical fleet/repo grouping (observability)
	Units        []UnitStatus         `json:"units"`                  // every reservable unit and its holder
	Shared       []SharedResourceInfo `json:"shared,omitempty"`       // host-level shared resources
	Provisioning *ProvisioningNetwork `json:"provisioning,omitempty"` // onboarding network the host advertises, if any
	LeaseSeconds int                  `json:"lease_seconds"`          // heartbeat lease window
	FreeUnits    int                  `json:"free_units"`             // units immediately available
	QueueLength  int                  `json:"queue_length"`           // waiters not yet assigned a unit
}

// Error is the JSON error envelope for non-2xx responses.
type Error struct {
	Error string `json:"error"`
}
