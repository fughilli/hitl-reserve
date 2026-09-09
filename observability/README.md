# Observability

The reservation daemon exposes Prometheus metrics at `GET /metrics` (same port as
the API). This directory has the collector config and a Grafana dashboard, built so
that **HITL fleets from multiple repositories can share one Grafana tenant** and be
viewed together in one dashboard or split into per-workspace dashboards.

## The label model — how multiple repos coexist

Every metric series carries two identifying labels, emitted by the daemon itself:

- **`host`** — the individual rig/machine (`--host`, defaults to the hostname).
- **`workspace`** — a logical grouping, set per fleet (`--workspace`, or the
  catalog's `workspace`). Use one workspace per repository/project (e.g.
  `splanc`, `toxc`, `tentacle-lamp`).

Because the labels are on the series, **any number of repositories can point their
Alloy collectors at the same Grafana Cloud tenant** with no coordination. In
Grafana you then either:

- **one shared dashboard, all repos**: the [`hitl-fleet`](dashboards/hitl-fleet.json)
  dashboard has a `workspace` multi-select variable — pick one, several, or all;
  every panel filters by it and breaks out per `host`. This is the "single
  dashboard across repositories" view.
- **per-workspace dashboards**: import the same JSON per repo and set the
  `workspace` variable's default (or make it a constant) to that repo — a
  dedicated dashboard per project in the same Grafana workspace.

No per-rig or per-repo dashboard edits are needed either way; the variables do the
slicing.

## Metrics

| Metric | Type | Meaning |
| --- | --- | --- |
| `hitl_up` | gauge | 1 while the daemon serves. |
| `hitl_units_total` | gauge | Reservable units on the host. |
| `hitl_units_busy` | gauge | Units with an active reservation. |
| `hitl_unit_busy{unit,unit_type}` | gauge | Per-unit occupancy (0/1). |
| `hitl_queue_depth` | gauge | Reservations queued waiting for a unit. |
| `hitl_active_reservations` | gauge | Active reservations (one per busy unit). |
| `hitl_lease_seconds` | gauge | Heartbeat lease window. |
| `hitl_reservations_total` | counter | Reservations enqueued. |
| `hitl_activations_total` | counter | Queued→active transitions. |
| `hitl_releases_total` | counter | Reservations ended (any reason). |
| `hitl_lease_expirations_total` | counter | Reservations reaped for a lapsed lease. |
| `hitl_start_failures_total` | counter | Environment start failures during reconcile. |
| `hitl_host_load1` / `hitl_host_memory_*` / `hitl_host_temperature_celsius` | gauge | Host resources. |

Every series is labeled `host` and `workspace`; `hitl_unit_busy` adds `unit` and
`unit_type`.

Worth alerting on: nonzero `rate(hitl_start_failures_total)` (a unit that won't come
up), sustained `rate(hitl_lease_expirations_total)` (clients dying mid-session), or
`hitl_queue_depth` staying high (fleet under-provisioned for load).

## Collector (Grafana Alloy on each rig)

Rigs typically sit behind a private network (Tailscale/VLAN) and shouldn't accept
inbound scrapes, so the standard pattern is a collector **on each rig** that scrapes
locally and dials *out* to Grafana Cloud over HTTPS. [`alloy.alloy`](alloy.alloy)
does exactly that: scrape `127.0.0.1:8087/metrics`, `remote_write` to Grafana Cloud.

Because the daemon already stamps `host` and `workspace` on every series, the
collector needs no per-rig config — the same `alloy.alloy` runs on every rig. Set
the Grafana Cloud endpoint/credentials via the `GRAFANA_CLOUD_PROM_*` env vars
(seed them out-of-band, like any other secret).

## Dashboard

[`dashboards/hitl-fleet.json`](dashboards/hitl-fleet.json) — import into Grafana
(Dashboards → New → Import), pick your Prometheus datasource. Variables:

- **`workspace`** (multi) — which repositories/fleets to show.
- **`host`** (multi) — which rigs, filtered by the selected workspace(s).

Panels: fleet up/free/queued summary, queue depth and unit occupancy over time,
reservation throughput and failure/expiry rates, and host resources — all sliced by
the two variables. Edit panels in a PR and re-import (or wire a dashboards-as-code
sync job in your repo's CI).
