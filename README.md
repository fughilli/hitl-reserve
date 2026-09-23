# hitl-reserve

A generalized **hardware reservation and queueing system** for hardware-in-the-loop
(HITL) benches. It lets many clients (agents, CI jobs, developers) share a fleet of
physical hardware: they queue for a *unit*, get exclusive use of it with an SSH
session into a per-reservation environment, hold it with a heartbeat lease, and
release it for the next waiter.

It is **hardware-agnostic**: you declare what is reservable and in what units — a
single USB board, a network-attached device, a *composite* of several connected
devices (e.g. an ESP32-C6 wired to a HackRF One), or any mix — plus *shared
resources* (like a logic analyzer) that are multiplexed across units rather than
exclusively held.

Extracted from [`fughilli/splanc`](https://github.com/fughilli/splanc)'s
`pi/hitl` rig into a self-contained, reusable module. It is Go, **stdlib-only**
(no third-party dependencies), and builds with both `go` and Bazel.

## Model

```
 host (one machine running hitl-reserved)
 ├── unit  c6-a            (usb)        ← one USB board
 │    └── component c6-a   [flash jtag improv led-strip]
 ├── unit  c6-b+sdr        (composite)  ← several connected devices, reserved together
 │    ├── component c6-b   [flash jtag led-strip]
 │    └── component sdr-a  [sdr rx tx]
 ├── unit  pi-1            (network, pin-only)  ← LAN device, no local board
 │    └── component pi-1   [wss-app led-strip]  (address: pi1.local)
 └── shared logic-analyzer                      ← multiplexed across units, not reserved
```

- **Component** — one atomic piece of hardware wired to the host (a USB board, a
  network device, an SDR). Has device nodes, env, an address, and capabilities.
- **Unit** — the atomic thing a client reserves. Bundles one or more components,
  attached *together* into one environment. A single board is a one-component unit;
  a composite is a multi-component unit reserved atomically.
- **Capability** — a feature string a unit advertises (the union of its
  components' capabilities plus any from bound shared resources). Clients target a
  unit by **name**, by **type**, or by **required capabilities** (best-fit).
- **Shared resource** — a host-level instrument multiplexed across units (e.g. one
  logic analyzer whose channels tap several units' signals). Brokered and
  serialized, never exclusively reserved. See [`shared/`](shared/shared.go).

## Packages

| Package | Role |
| --- | --- |
| [`api`](api/api.go) | Wire types shared by daemon and client (dependency-free). |
| [`engine`](engine/engine.go) | The reservation state machine: FIFO admission, per-unit single-active-slot, heartbeat leases, capability/type best-fit placement, hot-plug reconcile. |
| [`catalog`](catalog/catalog.go) | Declarative JSON catalog → resolved units + shared registry. Source of truth. |
| [`runner`](runner/runner.go) | Pluggable execution backend (`Runner`); [`podman`](runner/podman.go) reference impl attaches a unit's components and publishes an sshd port. |
| [`shared`](shared/shared.go) | Shared-resource broker framework; [`shared/analyzer`](shared/analyzer/analyzer.go) is a full example (logic analyzer). |
| [`discovery`](discovery/discovery.go) | Optional live USB auto-discovery of boards. |
| [`pool`](pool/pool.go) | Client-side multi-host selection ($HITL_HOSTS). |
| [`metrics`](metrics/metrics.go) | Prometheus text exposition (stdlib). See [observability](observability/). |
| [`cmd/hitl-reserved`](cmd/hitl-reserved/main.go) | The reservation daemon. |
| [`cmd/hitl`](cmd/hitl/main.go) | The client CLI. |

## Quickstart

Build:

```bash
go build ./...           # or: bazel build //...
go test ./...            # or: bazel test //...
```

Write a catalog describing your bench (see [`config/example.json`](config/example.json)),
then run the daemon:

```bash
hitl-reserved --catalog config/example.json --addr :8087 \
  --host $(hostname) --image my-test-env:latest
```

Reserve from a client:

```bash
export HITL_HOSTS="rig-1,rig-2"        # comma/space list; bare host → http://host:8087

hitl status                            # show every host's units and shared resources
hitl reserve --type esp32c6            # queue for any free ESP32-C6, SSH in, release on exit
hitl reserve --caps sdr,led-strip      # queue for a unit with these capabilities (best-fit)
hitl reserve --unit c6-b+sdr -- ./run-my-test.sh   # pin a composite; run a command; release
hitl shared logic-analyzer --unit c6-a '{"op":"capture"}'   # use a shared resource

hitl maintenance                       # cordon the host and wait until it drains
hitl maintenance --status              # show the current cordon/drain state
hitl maintenance --release             # lift the cordon; queued reservations resume
```

The client generates a throwaway SSH keypair per reservation, authorizes it on the
unit's environment, heartbeats to hold the lease, and releases on exit. See
[`docs/DESIGN.md`](docs/DESIGN.md) for the environment image contract.

### Maintenance mode

To take a host out of service (e.g. to redeploy the daemon), `hitl maintenance`
**cordons** it: queued reservations stop activating (new reserves still queue and
hold their position) while active reservations keep running and **drain** on release
or lease expiry. The command polls until the host is fully drained. The cordon
persists across a daemon restart — it writes a marker at `<state-dir>/maintenance`
and the daemon reads it on startup — so the host comes back still cordoned until
`hitl maintenance --release` lifts it. `/status` reports `cordoned`/`draining`, and
`GET /maintenance` returns `{cordoned, active, queued, drained}`. Endpoints:
`POST /maintenance` (enter; `?wait=1` blocks until drained), `GET /maintenance`
(status), `POST /maintenance/release` (leave).

## Declaring your hardware

The catalog is declarative JSON. The four hardware scenarios in one file:

```jsonc
{
  "resource_types": {
    "esp32c6":   { "kind": "usb",     "capabilities": ["flash","jtag","led-strip"] },
    "hackrf-one":{ "kind": "usb",     "capabilities": ["sdr","rx","tx"] },
    "pi-player": { "kind": "network", "capabilities": ["wss-app","led-strip"] }
  },
  "shared_resources": [
    { "name": "logic-analyzer", "kind": "logic-analyzer",
      "config": { "driver": "fx2lafw", "samplerate": "24m" } }
  ],
  "components": [
    { "name": "c6-a",  "type": "esp32c6",  "devices": ["/dev/serial/by-id/…:/dev/ttyACM0"] },
    { "name": "sdr-a", "type": "hackrf-one" },
    { "name": "pi-1",  "type": "pi-player", "address": "pi1.local" }
  ],
  "units": [
    { "name": "c6-a",     "type": "esp32c6",        "components": ["c6-a"],
      "shared": [{ "resource": "logic-analyzer", "config": { "channels": ["D0"], "protocol": "ws2812" } }] },
    { "name": "c6-a+sdr", "type": "esp32c6+hackrf", "components": ["c6-a","sdr-a"] },
    { "name": "pi-1",     "type": "pi-player",      "pin_only": true, "components": ["pi-1"] }
  ],
  "discovery": { "enabled": true, "type": "esp32c6", "glob": "/dev/serial/by-id/usb-Espressif_*-if00" }
}
```

- A **composite** unit lists several `components`; they are attached together and
  the unit advertises the union of their capabilities.
- A **shared resource** is declared once and *bound* per unit (its `config` is
  broker-defined — for the analyzer, the channels that tap that unit); the binding
  contributes capabilities the unit advertises so tests can select on them.
- **`pin_only`** keeps scarce/fragile units (e.g. the one network device) out of
  bare "any unit" / capability-only requests; reach them with `--unit`/`--type`.
- **`discovery`** layers runtime-detected units on top of the static catalog: USB
  boards by `glob`, and/or units listed in a `seeded_file` (the general way to
  attach a unit the host can't auto-detect — e.g. a network device — by editing a
  JSON file on the running host, no redeploy).
- **`provisioning_network`** advertises an onboarding network (SSID/PSK) in
  `/status` so a holder can provision a DUT onto it with no out-of-band creds; a
  per-host SSID can be supplied at runtime via `--provisioning-ssid/-psk`.

See [`docs/DESIGN.md`](docs/DESIGN.md) for the placement rules and how to add a new
shared-resource kind or a new runner backend.

## Observability

The daemon exports Prometheus metrics at `GET /metrics`, every series labeled with
`host` **and** `workspace`. The `workspace` label is what lets **several
repositories' fleets remote_write into one Grafana tenant** and be viewed together
in one dashboard (filter/repeat by `workspace`) or split into per-workspace
dashboards. See [`observability/`](observability/README.md).

### Status pages

Alongside the JSON API the daemon serves human-readable, self-refreshing HTML:

- `GET /status.html` — host overview: every unit (free/busy), each active
  reservation linking to its own page.
- `GET /reservation/{id}/status.html` — one reservation's live view: uptime,
  state, owner, unit (or queue position while waiting), endpoint, its annotations
  (rendered as links when the value is a URL), and its scratchpad. It carries a 5s
  meta-refresh and a form to append a scratchpad note.

Each reservation also carries two free-form fields, exposed in the `Reservation`
JSON (`/status`, `/reservation/{id}`) and mutable over HTTP:

- **scratchpad** — an ordered list of timestamped `{time, author, text}` notes the
  holder appends to record what it is doing.
  `POST /reservation/{id}/scratchpad` accepts JSON (`{"author":…,"text":…}`) or a
  url-encoded form; a form post 303-redirects back to the status page, an API post
  returns the updated reservation JSON.
- **annotations** — a `map[string]string` a deployment attaches per reservation
  (the daemon does not interpret keys). Set one with
  `POST /reservation/{id}/annotation` — JSON `{"key":…,"value":…}` — e.g. to attach
  a URL to an external view that the status page then renders as a link.

## Reusing this as a Bazel module

The repo is a bzlmod module (`hitl_reserve`). Depend on it from another Bazel repo
via a `git_override` in your `MODULE.bazel`, then reference the public targets
(`@hitl_reserve//engine`, `@hitl_reserve//cmd/hitl-reserved`, …) or import the Go
packages directly. Because the code is stdlib-only there is no Go dependency graph
to sync. Regenerate BUILD files after editing with `bazel run //:gazelle`.

## Provenance

The reservation engine, capability/pin placement rules, podman runner USB
isolation, the shared logic-analyzer broker, and the metrics exposition are
generalized from the battle-tested `pi/hitl` rig in `fughilli/splanc` (proven on a
real Raspberry Pi + ESP32-C6 fleet). This repo removes the splanc-specific
hardware assumptions and makes the resource model, shared resources, and runner
backend pluggable and declarative. See [`docs/DESIGN.md`](docs/DESIGN.md).
