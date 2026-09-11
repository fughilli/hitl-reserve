# Design

`hitl-reserve` is a hardware reservation broker. This document covers the domain
model, the placement algorithm, the extension points (runners and shared
resources), the environment-image contract, and the security posture.

## Domain model

A **host** runs one `hitl-reserved` daemon and offers reservable **units**. A unit
is composed of one or more **components** (individual pieces of hardware). A unit is
what a client reserves; its components are attached *together* into a single
per-reservation environment.

```
Catalog (declarative JSON)                Runtime
────────────────────────────             ─────────────────────────────
resource_types  ─┐                        engine.Manager
components  ─────┼── catalog.Resolve ──►    ├── []runner.Unit  (placement)
units  ──────────┤                          └── shared.Registry
shared_resources ┘                        runner.Runner (podman)  ── environments
discovery (optional) ── discovery.Monitor ─► Manager.SyncUnits (hot-plug)
```

Why "unit" and "component" rather than "DUT": a device-under-test is one use; the
reservable thing may be a bundle (board + SDR + power supply) or a bare network
host. The engine only cares about *units* and their *capabilities*; what a unit is
made of is a catalog/runner concern.

### Capabilities

Every unit advertises a set of capability strings — the union of:

1. its components' capabilities (from each component's resource type + extras),
2. unit-level extras declared in the catalog, and
3. capabilities contributed by **bound shared resources** (e.g. a unit tapped by
   the logic analyzer advertises `logic-analyzer-led-strip`).

Clients never name a driver or a wiring detail; they request capabilities and the
engine finds a unit that has them. Adding a capability to a resource type makes
every unit of that type match the tests that need it, with no per-test change.

## Placement

`engine` runs a FIFO admission queue. Each unit has a single active slot. A waiter
is admitted to the *best-fitting* free unit that can serve it. `unitServesLocked`
decides eligibility:

- **type pin** (`unit_type`): the unit's type must match.
- **capabilities** (`require_caps`): the unit's capabilities must be a superset.
- **name pin** (`unit`): if set, only that exact unit; otherwise—
- **pin-only**: a `pin_only` unit is reachable *only* by an explicit name or type
  target, never by a bare "any unit" or a capability-only request. This keeps
  scarce or fragile units (the single network device, a shared-instrument-wired
  board) out of ordinary work.

Among eligible free units, **best-fit** picks the one with the *fewest capabilities
beyond what the request requires* — so an over-provisioned unit (say, the only one
wired to the logic analyzer) stays free for the work that actually needs it. Ties
fall through to admission order.

The queue is **waiter-driven**: it scans reservations in admission order and gives
each the tightest free unit it can use, so a waiter needing a specific unit's
capabilities activates even while an earlier free unit sits idle with no compatible
work. A batch of reservations fills every free unit in one reconcile pass; a failed
start drops that reservation (surfaced to the client as `released` with a message)
and the pass continues so one bad unit can't strand the queue.

### Host concurrency cap

`--max-concurrent N` (`WithMaxConcurrent`) bounds how many reservations may be
active on a host *at once*, independent of unit count. It's a host-level policy for
protecting a weak SBC or a shared resource (USB bus, radio, CPU) from N-wide load:
a host with four units but `--max-concurrent 2` keeps at most two environments
running, and the rest wait in the admission queue and start as active ones release.
`0` (default) is unlimited — concurrency is bounded only by the unit count. This is
peak-concurrency shaping, not admission fairness: FIFO order and best-fit placement
are unchanged; only the number of simultaneously-live slots is capped.

### Leases and reaping

A reservation carries a lease. The holder (and queued waiters) heartbeat to extend
it; `ReapExpired`, called periodically, releases any reservation whose lease lapsed
— active holders (tearing their environment down) and queued waiters (dequeuing
them) alike. Sweeping the *whole* queue, not just the head, is what keeps a dead
client from stranding a slot indefinitely.

### Hot-plug

`Manager.SyncUnits` reconciles the live unit set: it adds newly-attached units,
drops detached idle ones (dequeuing anything pinned to them), preserves in-place
units and their live environments, and adopts a changed spec on an *idle* unit. A
*busy* unit is retained even if it momentarily drops out of discovery (a board that
re-enumerates on reset), and pruned only once idle. `discovery.Monitor` drives this
from a USB-by-id scan.

## Extension point: Runner

`runner.Runner` is the execution backend — what "activate a unit" means. The
reference `PodmanRunner` launches an OCI container with:

- the unit's sshd published on its port, and the holder's key mounted read-only;
- every component's `--device` nodes mapped in; a network component's `Address`
  injected as env (resolved to an IP host-side via `getent`, since the environment
  may lack mDNS);
- **per-unit raw-USB isolation** (`RawUSB`): rather than a whole-bus mount (which
  leaks every board on the host into every container — fatal for a multi-unit
  host), each unit gets a private `/dev/bus/usb` tree holding *only its own boards'
  nodes*, kept in sync across the boards' re-enumerations. Falls back to a whole-bus
  mount when a board's port can't be resolved (e.g. non-hardware reservations).

Implement `Runner` to target something else (bare-metal exec, a VM, a remote lab).
The engine only needs `Start`/`Stop`/`Cleanup`.

## Extension point: shared resources

`shared.Broker` models an instrument multiplexed across units rather than reserved.
It is **not** placed by the engine; any active holder can use it, and the broker
serializes access to the single device internally. Register brokers in a
`shared.Registry`; the daemon exposes them at `POST /shared/{name}` (a generic
passthrough — the broker owns the request/response schema) and advertises them in
`/status`.

A broker may also implement `shared.Binder`: a unit *binds* a slice of the resource
(broker-defined config — for the analyzer, the channels that tap that unit), and the
binding returns the capabilities the unit then advertises. This is how "unit X is
wired to analyzer channel D0" becomes the `logic-analyzer-led-strip` capability the
placement engine matches on.

[`shared/analyzer`](../shared/analyzer/analyzer.go) is a complete example: a logic
analyzer whose channels tap several units, mapping each unit to its channel subset +
wire protocol, running triggered `sigrok` captures, decoding them, and serializing
on the one instrument. To add a new kind (a programmable power supply, a spectrum
analyzer, a switched RF matrix), implement `Broker` (+ optionally `Binder`) and
register a `catalog.BrokerFactory` for its `kind` in the daemon.

> Note: capabilities from a *catalog* binding are advertised whether or not the
> instrument is physically present; a capture against an absent instrument returns a
> 503 at runtime. Live *discovery* enrichment (`TapCaps`) only adds the capability
> when the broker reports present, matching splanc's "rig wiring is per-board"
> behavior.

## Environment image contract

The podman runner starts your `--image`. It must:

1. run an sshd on container port 22;
2. install the OpenSSH public key mounted at `/run/hitl/authorized_keys` for the
   login user named by `$HITL_SSH_USER` (default `agent`);
3. (optionally) use the injected env: `HITL_UNIT`, `HITL_UNIT_TYPE`,
   `HITL_COMPONENTS`, `HITL_ADDRESS[_<COMPONENT>]` (network components),
   `HITL_BROKER_URL` (base URL to reach `/shared/{name}` for shared resources), and
   any per-component/unit env from the catalog.

A minimal entrypoint: write `$HITL_SSH_USER`'s `~/.ssh/authorized_keys` from the
mount, ensure host keys, `exec /usr/sbin/sshd -D`.

## Security posture

The daemon speaks plain HTTP and authenticates nothing itself — it is designed to
listen on a **trusted network** (a Tailscale tailnet, a lab VLAN), the same posture
as the origin splanc rig. Bind `--addr` to that interface. Access control, TLS
termination, and audit belong to that layer. The per-reservation SSH key is the
only credential that reaches into an environment, and it is authorized for exactly
the lease duration.

## Deliberate non-goals / simplifications vs. splanc

- **stdlib-only**: no YAML (JSON catalog), no `client_golang` (hand-rolled
  exposition). Keeps the module dependency-free and trivial to re-vendor.
- Dropped splanc-specific bits that don't generalize: the ESP-specific BLE/HCI
  per-reservation capture, the NetworkManager AP controller (generalized to the
  `engine.Hook` interface — wire an AP up/down around host activity), and the
  `sigrok` `.sr` reset-synthesis WS2812 decode workaround (the decode path is real;
  that one FX2-trigger quirk fix from splanc `FUG-140` is noted but not ported).
- The runner backend and shared-resource kinds are pluggable rather than hardcoded.
