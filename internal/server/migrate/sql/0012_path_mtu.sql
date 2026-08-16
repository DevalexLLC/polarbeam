-- Path MTU measurements. All sizes are IP-packet bytes including the IP
-- header (the wire contract on PathMtuResult). Measurements live here and
-- in path_mtu_events, never as probe_results hypertable columns — the
-- traceroute precedent, keeping byte counts out of the latency families.

-- Latest usable measurement per series.
CREATE TABLE path_mtu_current (
    agent_id              uuid NOT NULL,
    probe_id              uuid NOT NULL,
    target_id             uuid NOT NULL,
    updated_at            timestamptz NOT NULL,
    largest_ok_bytes      int NOT NULL,
    smallest_failed_bytes int NOT NULL,      -- 0 = unknown (max passed)
    next_hop_mtu_bytes    int NOT NULL,      -- 0 = no ICMP-reported MTU
    ip_version            smallint NOT NULL, -- 4 or 6
    black_hole            boolean NOT NULL,
    local_constraint      boolean NOT NULL,
    rtt_us                int,               -- NULL = not measured
    PRIMARY KEY (agent_id, probe_id)
);
CREATE INDEX path_mtu_current_probe_idx ON path_mtu_current (probe_id);

-- One row whenever a series' measured MTU or black-hole state changes.
CREATE TABLE path_mtu_events (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    time           timestamptz NOT NULL,
    agent_id       uuid NOT NULL,
    probe_id       uuid NOT NULL,
    target_id      uuid NOT NULL,
    old_mtu_bytes  int NOT NULL,
    new_mtu_bytes  int NOT NULL,
    old_black_hole boolean NOT NULL,
    new_black_hole boolean NOT NULL
);
CREATE INDEX path_mtu_events_time_idx ON path_mtu_events (time DESC);
