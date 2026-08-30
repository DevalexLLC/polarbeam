# PolarBEAM sizing and resource requirements

This guide covers the hardware needed to run the PolarBEAM control plane and
agent hosts: minimum and recommended CPU, memory, disk, I/O, and network
bandwidth, plus the model behind those numbers so you can size your own
deployment instead of trusting a table.

Two kinds of numbers appear below and are labeled throughout:

- **Measured** values come from a steady-state reference deployment: one
  control-plane host running proxy, server, and TimescaleDB, with six meshed
  agents and roughly 15 days of probe history at the default cadences.
- **Modeled** values extrapolate from the measured constants using the sizing
  model in this guide. Larger tiers have not yet been load-tested; treat them
  as planning estimates and verify with the measurement queries at the end.

## Quick reference

### Control plane (proxy + server + TimescaleDB on one Docker host)

| Deployment | vCPU | RAM | Disk (SSD) | Network |
|---|---|---|---|---|
| Minimum / evaluation (≤10 sites) | 2 | 4 GB | 20 GB | < 1 Mbps |
| Medium (~50 sites, default cadences) | 4 | 8 GB | 150 GB | < 1 Mbps |
| Large (~100 sites, default cadences) | 4–8 | 16 GB | 500 GB | ~1 Mbps |

"Sites" assumes one agent per site in a full ICMP mesh at the default
30-second interval plus a 5-minute traceroute mesh and a modest set of direct
probes — the suggested initial workload in `probes.md`. Mesh cost grows
quadratically with site count, so regional meshes (see
[Large-infrastructure patterns](probes.md#large-infrastructure-patterns))
directly reduce every number in this table.

Use an SSD for the Docker volume storage. The database is the only component
with meaningful I/O; at the large tier it sustains a few hundred small inserts
per second plus periodic aggregate refreshes, which spinning disks handle
poorly.

### Agent host

| Resource | Requirement |
|---|---|
| CPU | Negligible: well under 5% of one core (measured under 1%) |
| Memory | 64 MB minimum; 128 MB is a comfortable container limit (measured ~11 MB) |
| Disk | 1 GB free: ~45 MB image + up to 256 MB spool (default cap) + headroom |
| Network | Kilobytes per second at typical workloads; see the model below |

The agent is a single static Go binary in an Alpine container. It has no
runtime dependencies and its disk use is bounded by design: the on-disk spool
that buffers results while the server is unreachable is capped at
`spool.max_bytes` (default 256 MiB) and `spool.max_age` (default 7 days).

One requirement is strict: **the agent treats a spool write failure as fatal**
and exits rather than silently dropping measurements. A full disk on the
agent's state volume stops the agent. Keep free-space headroom above the spool
cap on the filesystem backing `/var/lib/polarbeam-agent`.

## The sizing model

Almost everything reduces to one number — **results per day** — which you can
compute from the workload you configure:

```text
results/day = directional assignments × (86,400 / interval seconds)
```

A full mesh of N single-agent sites creates `N × (N − 1)` directional
assignments per probe template (each direction is measured independently).
A direct probe adds one assignment per agent on its network at its source
site — every such agent runs it. Sites with multiple agents likewise
multiply their mesh assignments, since mesh expansion is per same-network
agent pair, not per site pair. Networks partition the quadratic: a mesh
pairs only agents on its own network, so N is the mesh's agent count per
plane, never the whole deployment — two 50-site meshes on separate networks
cost two 50-site meshes, not one 100-site one.

Worked examples for one ICMP template at the default 30-second interval:

| Sites (full mesh) | Assignments | Results/day | Inserts/sec |
|---|---|---|---|
| 10 | 90 | ~260,000 | ~3 |
| 50 | 2,450 | ~7,100,000 | ~82 |
| 100 | 9,900 | ~28,500,000 | ~330 |

A 5-minute traceroute mesh adds a tenth of the ICMP result volume; direct
probes add one result per interval each. For most deployments the ICMP mesh
dominates and the other templates are rounding error.

## Control plane

### Disk

The database is effectively the entire disk story, and its size is bounded by
retention, not by uptime: raw results are kept 14 days, hourly rollups 100
days, and daily rollups 400 days, after which TimescaleDB drops old chunks.
The database therefore grows to a steady-state plateau and stays there as long
as the workload is stable.

Measured constants from the reference deployment:

- One raw result costs **~500 bytes on disk** (hypertable plus its two
  indexes, stored uncompressed).
- With the default cadences, the retained rollups add roughly half of the raw
  footprint again, giving a steady-state total of about **22 days' worth of
  raw growth**.

So the planning formula is:

```text
raw growth/day    ≈ results/day × 500 bytes
steady-state size ≈ raw growth/day × 22
```

The 22× factor is tied to the fast-cadence probe mix it was measured under
(dominated by 30–60 second intervals). Rollup volume scales with the number
of probe series and their long retention — one hourly row per series for 100
days, one daily row for 400 — not with how often each series produces raw
results. A workload dominated by slow cadences (intervals of several minutes
or more) produces few raw rows per series but just as many rollup rows, so
the rollups can outweigh the raw data and the 22× shortcut underestimates.
For such workloads, measure the aggregate sizes directly with the query in
the last section instead of applying the factor.

Modeled examples: 10 sites ≈ 130 MB/day ≈ 3 GB steady state; 50 sites ≈
3.5 GB/day ≈ 80 GB; 100 sites ≈ 14 GB/day ≈ 310 GB. The quick-reference tiers
add headroom above these for WAL, temporary bloat before retention jobs run,
and growth in the workload.

Budget separately for images and fixed data: the server, agent, and proxy
images total under 150 MB, but the TimescaleDB image is ~4.7 GB. Traceroute
paths, path-MTU results, and outage events are stored on change only and are
negligible next to the results hypertable. Login events are retained
indefinitely by design; at interactive-login volumes this stays in the
megabytes.

### Memory

Measured at the reference scale: the Go server uses ~15 MB and the nginx
proxy under 100 MB. Both are rounding error.

The database is the memory consumer, with one behavior worth knowing: at
first initialization the `timescaledb-ha` image runs `timescaledb-tune`,
sizing `shared_buffers` and related settings from the container's memory
limit — or, since the shipped compose files set no limit, from the **host's
total RAM**. On a dedicated 4–16 GB control-plane host that is what you want.
On a large shared host, add a memory limit to the `timescaledb` service (or
set `TS_TUNE_MEMORY`) **before the first start**: the tuning runs only once,
so a limit added to an already-initialized deployment caps a database still
configured for the full host, which invites the kernel's out-of-memory
killer. In that case re-run `timescaledb-tune` inside the container (or set
the values manually) when adding the limit. Cap generously — a few gigabytes
at the medium tier — since undersized PostgreSQL memory converts into disk
I/O.

### CPU

Measured at the reference scale, every control-plane container idles below 1%
of one core. Two things scale CPU with fleet size:

- Ingest: hundreds of small inserts per second at the large tier, which is
  light work for PostgreSQL on an SSD.
- Config distribution: each connected agent's stream ticks every 30
  seconds, but a full snapshot rebuild happens only after a configuration
  write (an in-process change counter short-circuits unchanged ticks, with
  a forced rebuild every 5 minutes as a backstop for out-of-band SQL
  edits). Steady state is a cheap liveness/revocation check per agent; the
  rebuild burst after a config change is linear in agents × assignments,
  which is the reason the large tier recommends more cores than ingest
  alone would justify.

### Network

Trivial at every tier. A probe result is a ~150-byte protobuf message,
batched (up to 500 results or 5 seconds per batch) over each agent's single
mTLS gRPC stream. At the 100-site tier the aggregate ingest is on the order
of 100 KB/s including framing — around 1 Mbps — plus keepalives and
occasional config snapshots (sent only when configuration actually changes).
Dashboard traffic depends on operator use and is comparable to any small web
application.

### Disk I/O

Measured at the reference scale the database averages well under 1 MB/s of
writes, dominated by WAL, checkpoints, and the periodic aggregate-refresh
jobs rather than by ingest itself. Write load grows with ingest but remains
modest at the large tier. The practical guidance: any datacenter SSD is fine;
network or spinning storage with high sync latency is the only configuration
likely to struggle, because every batch insert and WAL flush pays that
latency.

## Agent host

The agent's footprint is fixed and small; only its network traffic scales
with the workload, and slowly.

**CPU and memory** (measured): ~11 MB RSS and under 1% of a core for an agent
running a six-site mesh workload. Probing is timers and tiny packets; even
hundreds of assignments do not change the picture materially. A 128 MB
container memory limit leaves generous headroom.

**Disk**: the image is ~45 MB and the state volume holds the agent's
identity, certificates, and the result spool. The spool grows only while the
control plane is unreachable, up to `spool.max_bytes` (default 256 MiB —
enough for several days of a typical site workload). Provision ~1 GB free and
remember that a full filesystem is fatal to the agent, deliberately.

**Network**, per assignment per interval at the defaults:

- ICMP: 10 echo requests of ~36 bytes each, plus replies — under 1 KB per
  30-second run.
- Traceroute: at most 90 small UDP probes plus ICMP responses per 5-minute
  run.
- Path MTU: the expensive one per run (up to a few tens of KB of full-size
  probes), run at long intervals.
- TCP/TLS/HTTP/DNS/NTP: one connection or datagram exchange per run.
- Results uplink: ~150 bytes per result, batched over one persistent stream.

A site in a 100-site ICMP mesh both sends ~99 probe runs and answers ~99
per 30 seconds and still totals only a few KB/s. Bandwidth only becomes worth
planning for if you configure aggressive path-MTU or HTTP workloads over
constrained WAN links — and that is a workload decision, not an agent
requirement.

Mesh *targets* need no capacity planning for ICMP, traceroute, and path-MTU
templates: peers answer those from the kernel, with no PolarBEAM listener
involved. TCP, TLS, and DNS mesh templates are different — they probe a
service the operator must already be running at each peer's probe address,
and that service absorbs one connection or query per source agent per
interval. Size that service for the mesh fan-in like any other client load.

## Measuring your deployment

The tiers above are starting points. After a week of steady-state operation,
measure and re-plan with your own numbers — especially on air-gapped sites,
where your measurements are the only ones that exist.

Container CPU, memory, and I/O:

```sh
docker stats --no-stream
```

Database size, ingest rate, and your deployment's own bytes-per-result
constant (run on the control-plane host):

```sh
docker compose exec timescaledb psql -U polarbeam -d polarbeam -c "
  SELECT pg_size_pretty(pg_database_size('polarbeam'))    AS database_total,
         pg_size_pretty(hypertable_size('probe_results')) AS raw_results,
         approximate_row_count('probe_results')           AS raw_rows_estimate,
         round(hypertable_size('probe_results')::numeric /
               nullif(approximate_row_count('probe_results'),0)) AS bytes_per_result;"
```

The row count is a statistics-based estimate (accurate to within a few
percent on an autovacuumed table) so that the query stays cheap on a large,
busy hypertable; an exact `count(*)` would scan every retained chunk.

Daily growth (results ingested over the last 24 complete hours, counted from
the hourly rollup so the query does not scan a day of raw results):

```sh
docker compose exec timescaledb psql -U polarbeam -d polarbeam -c "
  SELECT sum(samples) AS results_last_24h
  FROM probe_results_hourly
  WHERE bucket >= date_trunc('hour', now()) - interval '24 hours'
    AND bucket <  date_trunc('hour', now());"
```

Multiply `results_last_24h × bytes_per_result × 22` for your projected
steady-state database size before the retention plateau is reached (the raw
hypertable stops growing after day 14; the hourly rollup after day 100).
That shortcut assumes a fast-cadence workload, as explained above; check the
rollup sizes directly, especially when slow-cadence probes dominate:

```sh
docker compose exec timescaledb psql -U polarbeam -d polarbeam -c "
  SELECT view_name,
         pg_size_pretty(hypertable_size(format('%I.%I',
           materialization_hypertable_schema,
           materialization_hypertable_name)::regclass)) AS size
  FROM timescaledb_information.continuous_aggregates;"
```

The hourly and daily rollups keep growing until days 100 and 400
respectively; project their growth over the remaining retention window and
add it to the raw plateau.

On agent hosts, check spool pressure inside the state volume:

```sh
docker exec <agent-container> du -sh /var/lib/polarbeam-agent/spool
```

The agent also reports a lifetime dropped-results counter to the server
whenever it has been forced to discard data. A growing counter is a signal
to check the agent log, which records the reason for each drop, before
changing anything: drops attributed to `max_bytes` mean the spool cap is
undersized for your real outage windows — raise it and the disk
provisioning with it. Drops attributed to `max_age` mean an outage outlasted
the 7-day retention window; raising `spool.max_age` is rarely the right
response, because the server only folds late-arriving results into its
rollups for about 8 days, and results older than that window would be lost
to the rollups anyway.
