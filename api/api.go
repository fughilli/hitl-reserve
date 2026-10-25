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

import (
	"strings"
	"time"
)

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
	// OwnerEmail is the verified identity of the human behind the reservation (e.g.
	// an SSO/directory email). Unlike the free-form Owner label it is meant to be an
	// attributable identity. Optional: when empty a deployment may best-effort resolve
	// it (e.g. from the client's tailnet identity) on the daemon side.
	OwnerEmail string `json:"owner_email,omitempty"`
	// Actor records whether the reservation was made by a person at a keyboard
	// ("human") or by an automated agent acting on that person's behalf ("agent") —
	// both carry the same OwnerEmail, so this is the only thing distinguishing them.
	// Empty is treated as "human".
	Actor string `json:"actor,omitempty"`
	// IdentitySource is populated by the daemon, never trusted from the client: it
	// records how OwnerEmail was established — "tailscale" (verified from the caller's
	// tailnet identity), "asserted" (taken from the request without independent proof,
	// e.g. an agent on tagged infrastructure), or "" (none). Any client-supplied value
	// is overwritten.
	IdentitySource string `json:"identity_source,omitempty"`
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
	// Priority is the reservation's scheduling class (see PriorityMergeQueue,
	// PriorityPRCI, PriorityDev). Empty or unknown means PriorityDev, so a client
	// that predates priorities (or a human at a keyboard) lands in the dev class. A
	// daemon that predates priorities ignores the field and stays plain FIFO.
	Priority string `json:"priority,omitempty"`
}

// Scheduling classes, highest first. Classes only reorder the QUEUE: an active
// reservation is never preempted, whatever arrives behind it. Lower classes AGE:
// a reservation of class C is ordered as though it had arrived AgingOffset(C)
// later than it did, so a higher class can overtake it only by arriving within
// that window — after it, the older reservation is ahead of every newcomer and
// cannot starve.
const (
	// PriorityMergeQueue: merge-critical CI — merge-queue (merge_group) runs and the
	// post-merge run on main. Nothing ages ahead of it.
	PriorityMergeQueue = "merge-queue"
	// PriorityPRCI: ordinary PR CI (and periodic CI runs).
	PriorityPRCI = "pr-ci"
	// PriorityDev: interactive / agent development reservations (the default).
	PriorityDev = "dev"
)

// PriorityClasses lists the scheduling classes, highest first.
var PriorityClasses = []string{PriorityMergeQueue, PriorityPRCI, PriorityDev}

// NormalizePriority maps a client-supplied class (or a common alias) to its
// canonical name. Empty or unrecognized values map to PriorityDev; ok reports
// whether the input was recognized (an empty input counts as recognized).
func NormalizePriority(s string) (class string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "merge-queue", "merge_queue", "mergequeue", "merge_group", "merge-group", "mq", "main", "post-merge":
		return PriorityMergeQueue, true
	case "pr-ci", "pr_ci", "prci", "ci", "pr", "pull_request", "periodic":
		return PriorityPRCI, true
	case "dev", "agent", "human", "":
		return PriorityDev, true
	default:
		return PriorityDev, false
	}
}

// Queue-blocking reasons reported in Reservation.Blocked.
const (
	// BlockedCordoned: the host is in maintenance mode; nothing activates until it is
	// uncordoned. Waiting here makes no progress.
	BlockedCordoned = "cordoned"
	// BlockedNoUnit: no unit on the host can ever serve this request (e.g. the pinned
	// unit or every unit of the requested type has been unplugged).
	BlockedNoUnit = "no-unit"
)

// Endpoint is where the holder connects once active. The reservation flow is
// transport-agnostic: the client reads Host/Port/User out of the response and
// never assumes a fixed port, so distinct units publishing distinct ports need no
// client changes.
type Endpoint struct {
	Host string `json:"host"` // reach the host (e.g. its tailnet name)
	Port int    `json:"port"` // published sshd port for this unit's environment
	User string `json:"user"` // login user inside the environment
}

// Note is one timestamped entry in a reservation's scratchpad — a free-form line
// an agent or user appends to record what it is doing while it holds the unit.
type Note struct {
	Time   time.Time `json:"time"`
	Author string    `json:"author,omitempty"`
	Text   string    `json:"text"`
}

// Reservation is the full server-side view of one reservation.
type Reservation struct {
	ID    string `json:"id"`
	Owner string `json:"owner"`
	// OwnerEmail is the verified identity of the human behind the reservation (see
	// ReserveRequest.OwnerEmail). Empty when none was supplied or resolved.
	OwnerEmail string `json:"owner_email,omitempty"`
	// Actor is "human" or "agent" (see ReserveRequest.Actor). Empty is treated as
	// "human".
	Actor string `json:"actor,omitempty"`
	// IdentitySource records how OwnerEmail was established: "tailscale" (verified),
	// "asserted" (client-supplied, unverified), or "" (none). See
	// ReserveRequest.IdentitySource.
	IdentitySource string `json:"identity_source,omitempty"`
	State          State  `json:"state"`
	// Position is 0 once active. While queued it is 1 + the number of queued
	// reservations ahead of this one (in scheduling order) that compete for at least
	// one of the same units, so 1 means "next in line for my units".
	Position int `json:"position"`
	// Priority is the effective scheduling class (see PriorityMergeQueue …). Always
	// set by a priority-aware daemon, so its presence tells a client the daemon also
	// reports the queue-progress fields below.
	Priority string `json:"priority,omitempty"`
	// EtaSeconds estimates, while queued, how long until this reservation activates:
	// a list-scheduling simulation over the competing units' remaining hold times and
	// the waiters ahead, using each unit type's recent mean hold. Nil when active or
	// when no estimate is possible (see Blocked). An estimate only — a higher class
	// arriving later can still move ahead (within its aging window).
	EtaSeconds *int `json:"eta_seconds,omitempty"`
	// ProgressAt is the last time the queue made progress for this waiter: its
	// Position dropped, or one of the units it can use was freed / attached. Starts
	// at CreatedAt. Never advances while the host is cordoned.
	ProgressAt *time.Time `json:"progress_at,omitempty"`
	// SinceProgressSeconds is now − ProgressAt by the DAEMON's clock (so a client
	// with a skewed clock can still judge a stall). 0 when active.
	SinceProgressSeconds int `json:"since_progress_seconds,omitempty"`
	// Blocked names why a queued reservation cannot currently progress at all
	// (BlockedCordoned, BlockedNoUnit), or "" when it is simply waiting its turn.
	Blocked   string     `json:"blocked,omitempty"`
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
	// Scratchpad is an ordered log of notes (oldest first) the holder appends to
	// describe its activity on the unit.
	Scratchpad []Note `json:"scratchpad,omitempty"`
	// Annotations are free-form key→value pairs a deployment attaches to a
	// reservation (e.g. a URL to an external view). Keys are deployment-defined; the
	// daemon stores and surfaces them without interpreting any particular key.
	Annotations map[string]string `json:"annotations,omitempty"`
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
	// MaxConcurrent is the host-wide cap on concurrently-active reservations (0 =
	// unlimited); MaxConcurrentPerClass the per-unit-class (unit type) caps layered
	// under it. Informational, so a client can tell "queued behind a cap" from
	// "queued behind busy units".
	MaxConcurrent         int            `json:"max_concurrent,omitempty"`
	MaxConcurrentPerClass map[string]int `json:"max_concurrent_per_class,omitempty"`
	// QueueByClass counts queued reservations per scheduling class (every class is
	// present, possibly 0). PriorityAgingSeconds is each class's aging offset. Both
	// are omitted by a daemon that predates priorities.
	QueueByClass         map[string]int `json:"queue_by_class,omitempty"`
	PriorityAgingSeconds map[string]int `json:"priority_aging_seconds,omitempty"`
	// Cordoned is true when the host is in maintenance mode: no queued reservation
	// will activate until it is uncordoned (existing active ones keep running).
	Cordoned bool `json:"cordoned,omitempty"`
	// Draining is true while cordoned with active reservations still running (i.e.
	// cordoned but not yet fully drained). Once drained it is false.
	Draining bool `json:"draining,omitempty"`
}

// Maintenance is the cordon/drain state returned by the maintenance endpoints.
type Maintenance struct {
	Cordoned bool `json:"cordoned"` // host is in maintenance mode
	Active   int  `json:"active"`   // reservations still active (draining)
	Queued   int  `json:"queued"`   // reservations waiting (held, not activating while cordoned)
	Drained  bool `json:"drained"`  // cordoned && active == 0
}

// ClassQueue is one scheduling class's slice of a QueueSummary.
type ClassQueue struct {
	Class             string `json:"class"`
	Queued            int    `json:"queued"`
	OldestWaitSeconds int    `json:"oldest_wait_seconds"` // 0 when nothing is queued
	AgingSeconds      int    `json:"aging_seconds"`       // ordered as if it arrived this much later
}

// TypeQueue is one unit type's slice of a QueueSummary: what a NEW reservation of
// that type would face right now.
type TypeQueue struct {
	Type  string `json:"type"`
	Units int    `json:"units"`
	Free  int    `json:"free"`
	// Queued counts queued reservations that could land on a unit of this type.
	Queued int `json:"queued"`
	// HoldEstimateSeconds is the recent mean time a reservation holds a unit of this
	// type (the ETA model's input).
	HoldEstimateSeconds int `json:"hold_estimate_seconds"`
	// EtaNewSeconds estimates, per scheduling class, how long a reservation of that
	// class for this type would wait if enqueued now. Omitted while cordoned.
	EtaNewSeconds map[string]int `json:"eta_new_seconds,omitempty"`
	// RetryAfterSeconds is the back-off hint for a DEV reservation: 0 means go ahead
	// and reserve; otherwise check again after this long instead of enqueueing (see
	// the README's back-off convention).
	RetryAfterSeconds int `json:"retry_after_seconds"`
}

// QueueSummary is GET /queue: a cheap, reservation-free view of the host's queue
// (depth per class, ETA per unit type) for clients deciding whether to enqueue.
type QueueSummary struct {
	Host        string       `json:"host"`
	Workspace   string       `json:"workspace,omitempty"`
	Time        time.Time    `json:"time"`
	Cordoned    bool         `json:"cordoned,omitempty"`
	QueueLength int          `json:"queue_length"`
	Classes     []ClassQueue `json:"classes"`
	Types       []TypeQueue  `json:"types"`
}

// Error is the JSON error envelope for non-2xx responses.
type Error struct {
	Error string `json:"error"`
}
