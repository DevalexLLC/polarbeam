-- Cross-process config change signal.
--
-- The admin CLI (polarbeam-server probe/mesh/target/...) writes from its
-- own short-lived process, so an in-memory change counter in the serving
-- process cannot see those writes — and the config streams' rebuild
-- short-circuit must (CLI changes are promised to converge within one ~30s
-- tick). Every store write path that can alter probe expansion bumps this
-- single row; each stream tick reads it (a point select on one row —
-- orders of magnitude cheaper than the full snapshot rebuild it gates).
-- Single-row shape as in dashboard_settings: seeded here so readers never
-- branch on absence.
CREATE TABLE config_version (
    id      boolean PRIMARY KEY DEFAULT true CHECK (id),
    version bigint NOT NULL DEFAULT 0
);
INSERT INTO config_version DEFAULT VALUES;
