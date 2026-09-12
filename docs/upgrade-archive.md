# Upgrade procedures for installations that predate a release boundary

The procedures in this file apply only to an installation that ran, or
whose control plane was initialized, on the older releases each section
names. Those releases are unsupported (see `SECURITY.md`, "Supported
versions"); the procedures are kept because an operator upgrading such an
installation may still have to cross the boundary. Every other upgrade is
the normal live upgrade in [the install guide](install.md#upgrades).

The text is preserved from the install guide of the release that
introduced each boundary, with only headings and links adjusted. It is
kept accurate for the current release but is not re-verified with every
release: read the current install guide alongside it, check the exit
status of every command, and stop at the first check that does not show
the expected result.

Contents:

- [Crossing the database major version (pg16 to pg18: v0.10.0 to v0.11.0)](#crossing-the-database-major-version-pg16-to-pg18-v0100-to-v0110)
- [Starting fresh instead of crossing the database major version](#starting-fresh-instead-of-crossing-the-database-major-version)
- [Moving an existing CA to ML-DSA-65 (CAs initialized before v0.5.0)](#moving-an-existing-ca-to-ml-dsa-65-cas-initialized-before-v050)

## Crossing the database major version (pg16 to pg18: v0.10.0 to v0.11.0)

Applies to an installation on v0.10.0 or earlier that is moving to
v0.11.0 or later. An installation older than v0.10.0 first follows the
[live upgrade](install.md#control-plane-live-upgrade) to exactly v0.10.0,
using that release's images and compose file, then runs this procedure to
reach v0.11.0, then live-upgrades to the latest release.

The release that moved the database image from
`timescale/timescaledb-ha:pg16-all` to `timescale/timescaledb-ha:pg18`
changed the PostgreSQL major version underneath the `dbdata` volume. A
PostgreSQL data directory is specific to its major version: the pg18 image
started against a pg16 volume refuses with
`FATAL: database files are incompatible with server`, and no in-place
conversion happens. Data crosses the boundary with a logical dump and
restore, which is the procedure below. It replaces the live-upgrade steps
for that one release; every later upgrade is a normal live upgrade again.

An installation whose history is not worth keeping can instead discard
its state and reinstall; see
[Starting fresh instead of crossing the database major version](#starting-fresh-instead-of-crossing-the-database-major-version).

The procedure is deliberately fail-fast. Check the exit status of every
command. If any command fails, or any check does not show the expected
result, **stop and go to the rollback step** — do not continue. Nothing is
deleted until its replacement has been verified, so a rollback from any
point recovers the original database.

Expect downtime: the dump and the restore each read or write the whole
database, and the raw results hypertable (bounded by its 14-day retention)
dominates. Agents keep probing throughout and spool results locally, but a
spool holds at most `spool.max_bytes` (256 MiB by default) before the
oldest results are dropped, so a window of many hours on a busy fleet loses
the earliest measurements. Free disk on the control-plane host must cover
the dump file plus a second copy of the `dbdata` volume.

1. **Preconditions.** The installation must be running **v0.10.0**, the
   last release on the pg16 image, with a healthy server:

   ```sh
   grep POLARBEAM_VERSION .env
   docker compose ps
   ```

   Older installations first follow the normal
   [live upgrade](install.md#control-plane-live-upgrade) to v0.10.0. That is what
   pins the schema to a known state: a server only serves when no
   migration is pending, so a healthy v0.10.0 server proves the migration
   ledger is complete, and the restored database needs exactly the new
   release's migrations and nothing older. It also means the job
   inventory below is complete as written: the columnstore compression
   jobs exist only from the release that added migration 0026, which
   runs after this procedure.

   Back up all volumes and configuration as described in
   [Backup scope](install.md#backup-scope). Then copy the running configuration
   aside — the rollback step relies on these copies:

   ```sh
   cp docker-compose.yml docker-compose.pg16.yml
   cp .env .env.pg16
   ```

   Obtain the new release's `docker-compose.yml` (from the bundle, or the
   repository at the release tag) but do **not** replace the running copy
   yet, and do not edit `.env` yet. `POLARBEAM_VERSION` controls only the
   PolarBEAM images; the database image is a literal line in the compose
   file, which is why the file itself changes in this upgrade.

   Note the compose project name: the shipped compose file sets
   `name: polarbeam`, so unless you changed that, the project is
   `polarbeam` and the database volume is `polarbeam_dbdata` regardless of
   the installation directory (`docker compose ls` and `docker volume ls`
   show the actual names). It is written `<project>` below. Getting it
   wrong is not caught by Docker: a `-v` mount of a volume that does not
   exist silently creates an empty one, which is why step 5 checks the
   volume exists before copying it.

2. **Stage the images** (no downtime). Online:

   ```sh
   docker pull timescale/timescaledb-ha:pg18
   docker pull ghcr.io/devalexllc/polarbeam-server:<new-version>
   docker pull ghcr.io/devalexllc/polarbeam-proxy:<new-version>
   ```

   Offline: load the new bundle's image archive and the separately
   transferred pg18 database image as in
   [section 2](install.md#offline-installation) (the bundle's `TIMESCALEDB-IMAGE`
   names the new digest, and the retag step now uses the `pg18` tag).
   Either way, confirm the database image is present before continuing:

   ```sh
   docker image inspect timescale/timescaledb-ha:pg18 >/dev/null && echo ok
   ```

3. **Record the source extension versions and prove the target can
   install them** (no downtime). A logical restore requires the same
   TimescaleDB extension version on both sides, and the Toolkit is matched
   the same way. Read the versions the running database has:

   ```sh
   docker compose exec timescaledb psql -X -U polarbeam -d polarbeam -Atc \
     "SELECT extname, extversion FROM pg_extension
      WHERE extname IN ('timescaledb', 'timescaledb_toolkit')"
   ```

   Then prove that the pg18 image can install exactly those versions, in a
   disposable container on an empty volume. The first-start scripts of the
   image create both extensions at their newest versions, so the check
   drops them and recreates them at the recorded versions — the same
   sequence step 7 performs on the real target. Substitute the two
   versions from the query above:

   ```sh
   docker run -d --name pb-pg18-check -e POSTGRES_PASSWORD=check \
     timescale/timescaledb-ha:pg18
   # Repeat until it prints 2 (the initial server has no TCP listener, so
   # this only succeeds once the final server is up with both extensions):
   docker exec pb-pg18-check psql -X -h 127.0.0.1 -U postgres -Atc \
     "SELECT count(*) FROM pg_extension WHERE extname IN ('timescaledb', 'timescaledb_toolkit')"
   docker exec pb-pg18-check psql -X -U postgres -Atc \
     "SELECT name, version FROM pg_available_extension_versions
      WHERE (name, version) IN (('timescaledb', '<source ts>'), ('timescaledb_toolkit', '<source toolkit>'))"
   docker exec pb-pg18-check psql -X -U postgres -c \
     "DROP EXTENSION timescaledb_toolkit; DROP EXTENSION timescaledb CASCADE"
   docker exec pb-pg18-check psql -X -U postgres -c \
     "CREATE EXTENSION timescaledb VERSION '<source ts>'"
   docker exec pb-pg18-check psql -X -U postgres -c \
     "CREATE EXTENSION timescaledb_toolkit VERSION '<source toolkit>'"
   docker exec pb-pg18-check psql -X -U postgres -Atc \
     "SELECT extname, extversion FROM pg_extension
      WHERE extname IN ('timescaledb', 'timescaledb_toolkit')"
   docker rm -f pb-pg18-check
   ```

   The availability query must print both rows and the final query must
   print the recorded versions. If a version is missing, update it on the
   running pg16 database first — each `ALTER EXTENSION` must be the first
   statement of a fresh session — then re-read the versions and repeat the
   disposable check:

   ```sh
   docker compose exec timescaledb psql -X -U polarbeam -d polarbeam -c "ALTER EXTENSION timescaledb UPDATE"
   docker compose exec timescaledb psql -X -U polarbeam -d polarbeam -c "ALTER EXTENSION timescaledb_toolkit UPDATE"
   ```

   If the pg16 image on the host cannot reach a version the pg18 image
   provides, stop here; the installation needs a newer pg16 image before
   this upgrade can proceed.

4. **Downtime begins: pause the policies and dump.** Stop the server and
   proxy; agents keep probing and spool locally. Leave the database
   running. Record each policy job's schedule state and pause them all so
   refresh and retention cannot change the data between the baseline count
   and the dump:

   ```sh
   umask 077
   docker compose stop server proxy
   docker compose exec -T timescaledb psql -X -U polarbeam -d polarbeam -Atc \
     "SELECT job_id, scheduled::text FROM timescaledb_information.jobs
      WHERE proc_name IN ('policy_refresh_continuous_aggregate', 'policy_retention')
      ORDER BY job_id" > jobs-pg16.txt \
     && [ -s jobs-pg16.txt ] && cat jobs-pg16.txt
   ```

   `umask 077` makes every file this procedure writes owner-only for the
   rest of the shell session. The dump is a complete copy of the database,
   including credential hashes and any OIDC client secret, so it must not
   be world-readable; if a dump from an earlier attempt already exists,
   remove it first (`rm -f polarbeam-pg16.dump`) rather than overwriting
   it in place with its old permissions.

   Continue only if that printed the job list (an empty file means the
   query failed, and nothing has been paused). Then pause the jobs and wait
   until no policy run is in flight:

   ```sh
   docker compose exec timescaledb psql -X -U polarbeam -d polarbeam -c \
     "SELECT alter_job(job_id, scheduled => false) FROM timescaledb_information.jobs
      WHERE proc_name IN ('policy_refresh_continuous_aggregate', 'policy_retention')"
   # Repeat until it prints 0:
   docker compose exec timescaledb psql -X -U polarbeam -d polarbeam -Atc \
     "SELECT count(*) FROM pg_stat_activity
      WHERE application_name IN (SELECT application_name FROM timescaledb_information.jobs
        WHERE proc_name IN ('policy_refresh_continuous_aggregate', 'policy_retention'))"
   ```

   A policy's background worker reports the job's own `application_name`
   (for example `Retention Policy [1002]`), which is what the wait query
   matches on.

   Record the inventory the acceptance check compares against, then dump
   with the running database's own client and confirm the archive is
   readable before anything else is touched:

   ```sh
   docker compose exec -T timescaledb psql -X -U polarbeam -d polarbeam -Atc \
     "SELECT 'probe_results', count(*) FROM probe_results
      UNION ALL SELECT 'sites', count(*) FROM sites
      UNION ALL SELECT 'agents', count(*) FROM agents
      UNION ALL SELECT 'users', count(*) FROM users
      UNION ALL SELECT 'schema_migrations', count(*) FROM schema_migrations
      UNION ALL SELECT 'hypertables', count(*) FROM timescaledb_information.hypertables
      UNION ALL SELECT 'caggs', count(*) FROM timescaledb_information.continuous_aggregates
      UNION ALL SELECT 'jobs', count(*) FROM timescaledb_information.jobs
        WHERE proc_name IN ('policy_refresh_continuous_aggregate', 'policy_retention')" > counts-pg16.txt \
     && [ -s counts-pg16.txt ] && cat counts-pg16.txt
   docker compose exec -T timescaledb pg_dump -U polarbeam -Fc polarbeam > polarbeam-pg16.dump
   docker run --rm -i timescale/timescaledb-ha:pg18 pg_restore --list < polarbeam-pg16.dump > /dev/null && echo dump-ok
   ```

   Both inventory commands write straight to a file and then test that the
   file is non-empty (piping through `tee` would hide a failed query
   behind `tee`'s own success). `pg_dump` prints one expected warning,
   `there are circular foreign-key constraints on this table:
   continuous_agg`, about TimescaleDB's own catalog; it is harmless for a
   full dump, and the restore below completes without it mattering. Any
   other `pg_dump` error, or a non-zero exit, is a failed step. The
   `pg_restore --list` pass must print `dump-ok`.

5. **Preserve the pg16 volume, then retire it.** Stop the database, copy
   its volume to a new one, verify the copy byte for byte, and only then
   remove the live volume so the pg18 image initializes a fresh cluster on
   the next start. The copy runs inside the pg18 image itself (it carries a
   shell and coreutils, so an offline host needs no extra image) as root:
   the image's default user is `postgres` (uid 1000) and a new volume is
   root-owned, and root's `cp -a` preserves the uid 1000 ownership the
   database files need.

   The copy must land in an empty volume: if a `<project>_dbdata_pg16`
   volume is left over from an earlier attempt, remove it first, because
   `cp -a` into a non-empty volume merges and leaves stale files behind
   that the `diff` then reports.

   ```sh
   docker compose stop timescaledb
   docker compose rm -f timescaledb
   docker volume inspect <project>_dbdata >/dev/null && echo source-volume-ok
   docker volume inspect <project>_dbdata_pg16 >/dev/null 2>&1 && docker volume rm <project>_dbdata_pg16
   docker volume create <project>_dbdata_pg16
   docker run --rm --user 0 --entrypoint cp \
     -v <project>_dbdata:/from:ro -v <project>_dbdata_pg16:/to \
     timescale/timescaledb-ha:pg18 -a /from/. /to/
   docker run --rm --user 0 --entrypoint diff \
     -v <project>_dbdata:/from:ro -v <project>_dbdata_pg16:/to:ro \
     timescale/timescaledb-ha:pg18 -r /from /to && echo copy-ok
   docker volume rm <project>_dbdata
   ```

   Continue past the first line only if it printed `source-volume-ok` (a
   wrong `<project>` fails there instead of copying an empty volume), and
   remove the volume only after `copy-ok`. Up to this point the original
   database is intact and the first rollback branch below simply restarts
   it.

6. **Switch to the new compose file and start the empty database.** Put
   the new release's `docker-compose.yml` in place, set `POLARBEAM_VERSION`
   in `.env` to the new release, and start only the database service. Its
   first start creates the `polarbeam` role and database from `.env`, with
   the same password as before.

   ```sh
   cp <path-to-new>/docker-compose.yml docker-compose.yml
   # .env: POLARBEAM_VERSION=v<new-version>
   docker compose up -d --wait timescaledb
   # Repeat until it prints 2:
   docker compose exec timescaledb psql -X -h 127.0.0.1 -U polarbeam -d polarbeam -Atc \
     "SELECT count(*) FROM pg_extension WHERE extname IN ('timescaledb', 'timescaledb_toolkit')"
   ```

   `--wait` returns on the `pg_isready` health check, which the temporary
   initialization server can also satisfy over the socket; the TCP query
   succeeds only against the final server, once initialization has
   created both extensions. In practice the first query usually already
   prints `2`; the loop is the guarantee, not an expected wait.

7. **Match the extension versions, then restore.** The fresh database has
   both extensions at the image's newest versions. Replace them with the
   versions recorded in step 3, each `CREATE` in its own session so the
   loader picks the requested library, verify the versions match the
   record, and restore. `pg_restore` must run without `-j` (parallel
   restore does not restore the TimescaleDB catalog correctly) and stops at
   the first error:

   ```sh
   docker compose exec timescaledb psql -X -U polarbeam -d polarbeam -c \
     "DROP EXTENSION timescaledb_toolkit; DROP EXTENSION timescaledb CASCADE"
   docker compose exec timescaledb psql -X -U polarbeam -d polarbeam -c \
     "CREATE EXTENSION timescaledb VERSION '<source ts>'"
   docker compose exec timescaledb psql -X -U polarbeam -d polarbeam -c \
     "CREATE EXTENSION timescaledb_toolkit VERSION '<source toolkit>'"
   docker compose exec timescaledb psql -X -U polarbeam -d polarbeam -Atc \
     "SELECT extname, extversion FROM pg_extension
      WHERE extname IN ('timescaledb', 'timescaledb_toolkit')"
   docker compose exec timescaledb psql -X -U polarbeam -d polarbeam -c "SELECT timescaledb_pre_restore()"
   docker compose exec -T timescaledb pg_restore -U polarbeam -d polarbeam \
     --exit-on-error --no-owner --no-privileges -Fc < polarbeam-pg16.dump && echo restore-ok
   docker compose exec timescaledb psql -X -U polarbeam -d polarbeam -c "SELECT timescaledb_post_restore()"
   ```

   The version query must print exactly the step-3 values before the
   restore starts, and the restore must print `restore-ok`. A clean
   restore prints nothing else (the pre-created extensions do not conflict:
   the dump creates them with `IF NOT EXISTS`). A restore that stopped on
   an error leaves a partial database: go to the second rollback branch.

8. **Update the extensions and statistics.** TimescaleDB refuses to update
   once its old library is loaded in a session, so each `ALTER EXTENSION`
   runs as the first statement of its own session:

   ```sh
   docker compose exec timescaledb psql -X -U polarbeam -d polarbeam -c "ALTER EXTENSION timescaledb UPDATE"
   docker compose exec timescaledb psql -X -U polarbeam -d polarbeam -c "ALTER EXTENSION timescaledb_toolkit UPDATE"
   docker compose exec timescaledb psql -X -U polarbeam -d polarbeam -c "ANALYZE"
   ```

9. **Accept or roll back.** Re-run the step-4 inventory query into a
   second file and compare. The restored database must have exactly what
   the source had — the comparison is against your own record, not against
   fixed numbers:

   ```sh
   docker compose exec -T timescaledb psql -X -U polarbeam -d polarbeam -Atc \
     "<the same inventory query as in step 4>" > counts-pg18.txt
   diff counts-pg16.txt counts-pg18.txt && echo inventory-ok
   ```

   Any difference means rollback. On `inventory-ok`, put each policy job
   back to the schedule state it had before step 4 — a policy that was
   deliberately disabled before the upgrade stays disabled — and confirm
   the result matches the record:

   ```sh
   while IFS='|' read -r id sched; do
     docker compose exec -T timescaledb psql -X -U polarbeam -d polarbeam -c \
       "SELECT alter_job($id, scheduled => '$sched'::boolean)" < /dev/null
   done < jobs-pg16.txt
   docker compose exec -T timescaledb psql -X -U polarbeam -d polarbeam -Atc \
     "SELECT job_id, scheduled::text FROM timescaledb_information.jobs
      WHERE proc_name IN ('policy_refresh_continuous_aggregate', 'policy_retention')
      ORDER BY job_id" > jobs-pg18.txt
   diff jobs-pg16.txt jobs-pg18.txt && echo jobs-ok
   ```

   Job ids are catalog data and survive the dump unchanged. The
   `< /dev/null` matters: `docker compose exec` forwards its standard input
   by default and would otherwise swallow the remaining lines of the loop.

10. **Finish as a normal upgrade.** Continue with steps 4 to 6 of the
    [live upgrade](install.md#control-plane-live-upgrade): `migrate` applies every
    migration file the restored ledger does not list — because step 1
    required v0.10.0, that is the new release's own migrations, possibly
    none, and it prints each one it applies. A failing `migrate` is a
    failed step: use the second rollback branch. Then `docker compose up
    -d` and verify. Keep `polarbeam-pg16.dump`, `docker-compose.pg16.yml`,
    `.env.pg16`, and the `<project>_dbdata_pg16` volume until the upgraded
    system has been verified end to end, and longer if in doubt; remove
    them afterwards (`docker volume rm <project>_dbdata_pg16`).

11. **Rollback.** Which branch applies depends on whether the original
    volume still exists:

    - **Failed at or before the `docker volume rm` in step 5** — the
      original `<project>_dbdata` volume is intact. Do **not** copy
      anything back. If the compose file or `.env` was already replaced,
      restore the copies (`cp docker-compose.pg16.yml docker-compose.yml`
      and `cp .env.pg16 .env`), start the old stack (`docker compose up
      -d`), restore the job schedule states with the loop from step 9 (the
      jobs are still paused from step 4), and remove the
      `<project>_dbdata_pg16` copy if one was created.
    - **Failed after the original volume was removed** (steps 6 to 10,
      including a failed `migrate`) — the verified `<project>_dbdata_pg16`
      copy is the source of truth:

      ```sh
      docker compose down
      cp docker-compose.pg16.yml docker-compose.yml
      cp .env.pg16 .env
      docker volume inspect <project>_dbdata >/dev/null 2>&1 && docker volume rm <project>_dbdata
      docker volume create <project>_dbdata
      docker run --rm --user 0 --entrypoint cp \
        -v <project>_dbdata_pg16:/from:ro -v <project>_dbdata:/to \
        timescale/timescaledb-ha:pg18 -a /from/. /to/
      docker run --rm --user 0 --entrypoint diff \
        -v <project>_dbdata_pg16:/from:ro -v <project>_dbdata:/to:ro \
        timescale/timescaledb-ha:pg18 -r /from /to && echo copy-ok
      docker compose up -d
      ```

      The conditional removal covers a failure in step 6 before Compose
      created the replacement volume. Then restore the job schedule states
      with the loop from step 9. The pg16 stack is back exactly as it was
      before step 5, and the procedure can be retried from step 4 after
      the cause is understood (step 5 replaces the leftover
      `<project>_dbdata_pg16` copy with a fresh one).

## Starting fresh instead of crossing the database major version

Applies to the same boundary as the previous section.

An installation that does not need its history — a pilot, a lab, or a site
that would rather re-enroll than dump and restore — can cross the pg18
boundary by discarding the control plane's state and reinstalling. This is
not a shortcut through the procedure above: it is a new installation that
happens to reuse the host, and it keeps **nothing**. Every dashboard user,
site, network, mesh, probe, target, threshold, and measurement is recreated
by hand, and every agent re-enrolls.

The reason is the trust boundary described under
[Backup scope](install.md#backup-scope). The Compose file declares three volumes:
`dbdata` (the pg16 data directory), `server-state` (the built-in CA private
key and the auto-issued gRPC server certificate), and `tls` (the dashboard
certificate and key). `docker compose down -v` removes all three. Keeping
`server-state` to spare the agents does not work: agent certificate records
live in the database, so an empty database orphans every agent even under
the old CA. Removing all three and re-enrolling is the only consistent
outcome. `.env`, `server.yaml`, and the installation directory are not
volumes and survive the reset.

1. **Decide about the history first.** After the volumes are gone, the
   only way back is a backup taken now, and restoring it later means the
   full [dump-and-restore procedure](#crossing-the-database-major-version-pg16-to-pg18-v0100-to-v0110),
   which needs the pg16 image still loaded on the host. Take a backup per
   [Backup scope](install.md#backup-scope) if there is any chance the data is
   wanted, and keep the pg16 image until that question is settled.

2. **Stage the images** exactly as in step 2 of the procedure above,
   including the pg18 database image, and confirm it is present:

   ```sh
   docker image inspect timescale/timescaledb-ha:pg18 >/dev/null && echo ok
   ```

3. **Update the configuration.** Replace `docker-compose.yml` with the new
   release's copy — the database image is a literal line in that file — and
   set `POLARBEAM_VERSION` in `.env` to the new release. The database
   password is applied when the new `dbdata` volume initializes, so this
   is also the one moment it can change freely; if you do change it, set
   it in both `.env` and the `db.url` line of `server.yaml`, which must
   agree. Nothing else in `server.yaml` needs to change.

4. **Confirm what is about to be removed.** The shipped compose file sets
   `name: polarbeam`, so the volumes are `polarbeam_dbdata`,
   `polarbeam_server-state`, and `polarbeam_tls`; check the actual names
   before proceeding:

   ```sh
   docker compose ps
   docker volume ls --filter name=polarbeam
   ```

5. **Remove the stack and its volumes.** This is the point of no return
   for everything the previous step listed:

   ```sh
   docker compose down -v
   docker volume ls --filter name=polarbeam
   ```

   The second command must print no volumes. If one remains (for example
   because the compose project name was changed at some point), remove it
   with `docker volume rm` before continuing — a pg16 `dbdata` volume left
   in place makes the pg18 image refuse to start, as described under
   [Troubleshooting](install.md#database-container-exits-with-database-files-are-incompatible-with-server).

6. **Reinstall the control plane.** Follow
   [section 4.3](install.md#43-install-the-dashboard-certificate) to put the
   dashboard certificate and key back into the recreated `tls` volume, then
   [section 5](install.md#5-initialize-and-deploy-the-control-plane) from the top:
   `migrate`, `ca init`, `up -d`, and `user add --admin`. Record the new
   CA fingerprint that `ca init` prints; every fingerprint recorded before
   the reset is now wrong. Recreate networks, sites, meshes, probes,
   targets, and thresholds as in [sections 7](install.md#7-create-an-enrollment-token)
   through [10](install.md#10-configure-probe-workloads).

7. **Re-enroll every agent into a fresh state volume.** Mint a token for
   each agent's site **and its original network** (the token carries the
   network; omitting `--network` on a multi-network deployment silently
   lands the agent on `default`). Then, on each agent host, stop and remove
   the container, remove its state volume, and enroll again exactly as in
   [section 8](install.md#8-deploy-a-container-agent) with the new fingerprint and
   the same `--probe-address`:

   ```sh
   docker stop -t 60 polarbeam-agent
   docker rm polarbeam-agent
   docker volume rm polarbeam-agent-state
   # continue at section 8.1 (new volume, new token, NEW fingerprint,
   # same --probe-address), then start the container per section 8.3
   ```

   Replacing the volume, rather than `identity retire`, is deliberate:
   the spool holds results tagged with probe and target identifiers from
   the old database. Those identifiers do not exist in the new one, so a
   drained spool would only add rows that nothing can display. Anything
   the agent measured between the reset and its re-enrollment is lost
   either way; treat the reset as day zero for measurements.

8. **Clean up.** Delete the backup copies (`docker-compose.pg16.yml`,
   `.env.pg16`, any `polarbeam-pg16.dump`) if the dump-and-restore
   procedure was started before switching to this one, and remove the
   pg16 database image once the decision in step 1 is final:

   ```sh
   docker image rm timescale/timescaledb-ha:pg16-all
   ```

## Moving an existing CA to ML-DSA-65 (CAs initialized before v0.5.0)

Applies to a control plane whose built-in CA was initialized before
v0.5.0, the first release whose `ca init` defaults to ML-DSA-65. Such a CA
is ECDSA and keeps working on every later release, so this cutover is
optional and deliberate; no normal upgrade requires it.

The first release whose built-in CA defaults to ML-DSA-65 changes nothing
for an existing installation by itself: the existing (ECDSA) CA keeps
loading, existing agents keep renewing, and freshly enrolled agents get
ECDSA keys to match it. Moving an **existing** deployment to a post-quantum
CA is a deliberate cutover, because the CA algorithm cannot be changed in
place — every enrolled agent chains to the old root and holds a key of the
old algorithm. Two paths are supported:

- **Fresh install / pre-production:** nothing to migrate. `ca init` creates
  an ML-DSA-65 root and agents enroll normally.
- **Clean cutover (below):** re-initialize the CA, then re-enroll every
  agent. Each agent is dark from its stop until its re-enrollment; results
  spool to disk in the meantime, so data loss is bounded by spool capacity.

A rolling, no-outage migration (both roots trusted simultaneously, agents
rekeying at their own pace) is not yet supported; it is tracked upstream as
a follow-up to the ML-DSA work.

Before starting, note:

- **Upgrade every agent's binary first.** Agent images built before the
  ML-DSA release cannot verify an ML-DSA CA or enroll against it at all.
  The normal [agent upgrade](install.md#agents) is safe to do ahead of the cutover:
  upgraded agents keep their ECDSA identity until re-enrolled. The server
  also refuses to sign a CSR whose key algorithm does not match the CA,
  so a stale binary fails loudly at enrollment instead of silently
  keeping a classical identity under the post-quantum root.
- The handshake grows by roughly 20 KB each way with ML-DSA certificates.
  This is negligible for the long-lived gRPC streams; it is only worth
  knowing on severely constrained links.
- The nginx proxy needs no changes (SNI passthrough never terminates TLS).

The cutover, on the control-plane host:

1. **Back up** (see [Backup scope](install.md#backup-scope)) — and note that this
   backup restores the *classical* CA: restoring it after the cutover
   un-does the migration and orphans every re-enrolled agent.

2. **Retire the old CA and initialize the new one.** `ca init` refuses to
   overwrite, so move the CA directory aside inside the `server-state`
   volume with `ca retire` (this also retires the old auto-issued gRPC
   server certificate, which lives in the same directory). It renames the
   directory to `ca.retired-<UTC timestamp>` and prints that path — note
   it, the wrap-up step below refers to it:

   ```sh
   docker compose stop server
   docker compose run --rm server ca retire \
     --config /etc/polarbeam/server.yaml
   docker compose run --rm server ca init \
     --config /etc/polarbeam/server.yaml
   docker compose up -d server
   ```

   Record the new `sha256:<hex>` fingerprint that `ca init` prints — every
   re-enrollment below uses it, and every fingerprint recorded before the
   cutover is now wrong.

   From this moment the whole fleet's uplink is down: existing agent
   certificates no longer verify. Agents keep probing and spool results
   locally until re-enrolled.

3. **Re-enroll each agent.** For every agent, issue a fresh token for its
   site **and its original network** (as in
   [section 7](install.md#7-create-an-enrollment-token)). The token carries the
   network: omitting `--network` on a multi-network deployment would land
   the re-enrolled agent on `default`, silently dropping it out of its
   previous meshes and tenant scope. List the current assignments first:

   ```sh
   docker compose exec timescaledb psql -U polarbeam -d polarbeam -c \
     "SELECT s.name AS site, n.name AS network, a.id FROM agents a
        JOIN sites s ON s.id = a.site_id
        JOIN networks n ON n.id = a.network_id ORDER BY s.name, n.name"
   ```

   ```sh
   docker compose exec server polarbeam-server token create \
     --config /etc/polarbeam/server.yaml \
     --site <site-name> --network <network-name> --ttl 24h
   ```

   Then on the agent host: stop the agent, move **only** its identity
   aside with `identity retire` (the spool stays and drains after
   re-enrollment), re-enroll with the new fingerprint exactly as in
   [section 8.2](install.md#82-enroll-into-the-persistent-volume), and recreate the
   container:

   ```sh
   docker stop -t 60 polarbeam-agent
   docker rm polarbeam-agent
   docker run --rm \
     --cap-add NET_RAW \
     --mount type=bind,src=/opt/polarbeam-agent/agent.yaml,dst=/etc/polarbeam/agent.yaml,readonly \
     --mount type=volume,src=polarbeam-agent-state,dst=/var/lib/polarbeam-agent \
     ghcr.io/devalexllc/polarbeam-agent:<version> \
     identity retire --config /etc/polarbeam/agent.yaml
   # re-enroll per section 8.2 (new token, NEW fingerprint, same
   # --probe-address), then recreate the container per section 8.3
   ```

   `identity retire` renames `/var/lib/polarbeam-agent/pki` to
   `pki.retired-<UTC timestamp>` and prints that path; nothing is deleted,
   and the retired directory can be moved back by hand if the cutover has
   to be abandoned. Run it only against a stopped agent — the running
   agent's certificate renewer writes into `pki`. The `--cap-add` is the
   usual exec-time requirement from section 8.2, not something the
   subcommand uses.

   Re-enrollment issues a new agent identity: history recorded under the
   old identity remains queryable, but continues under the new one — the
   same identity-replacement semantics as any re-enrollment.

4. **Verify the fleet and let every spool drain.** Confirm on the
   dashboard that every site reports again and that each agent's spooled
   backlog has drained (the last-update times catch up and stay current).
   This step must complete **before** step 5: spooled results reference
   the probe and target IDs they were measured under, and once a replaced
   identity's target is retired the server rejects and permanently drops
   any still-spooled results that reference it.

5. **Retire the replaced identities.** The old agent rows do not go away
   on their own, and they are not inert: mesh expansion includes every
   agent of a member site, so each replaced identity stays in the other
   agents' probe sets as a permanently dark peer (doubled mesh probes
   against the same address, phantom down directions, endless
   agent-offline warnings). There is not yet an agent-retirement CLI; as
   with revocation, retire each replaced identity in the database. Find
   the old IDs (every site now has two agents per network it serves; the
   old one has the older `last_seen_at`):

   ```sh
   docker compose exec timescaledb psql -U polarbeam -d polarbeam -c \
     "SELECT a.id, s.name AS site, n.name AS network, a.last_seen_at
        FROM agents a
        JOIN sites s ON s.id = a.site_id
        JOIN networks n ON n.id = a.network_id
        ORDER BY s.name, n.name, a.last_seen_at"
   ```

   Then, for each **old** agent ID:

   ```sh
   docker compose exec timescaledb psql -U polarbeam -d polarbeam -c "BEGIN;
     UPDATE outage_events SET closed_at = now()
       WHERE agent_id = '<old-agent-id>' AND closed_at IS NULL;
     DELETE FROM certificates WHERE agent_id = '<old-agent-id>';
     DELETE FROM targets WHERE agent_id = '<old-agent-id>';
     UPDATE join_tokens SET used_by_agent = NULL WHERE used_by_agent = '<old-agent-id>';
     DELETE FROM agents WHERE id = '<old-agent-id>';
     COMMIT;"
   ```

   The `outage_events` update closes the identity's open agent-offline
   event — the offline sweep only examines rows still present in
   `agents`, so an event left open here would stay open on the dashboard
   forever. Recorded measurement history and closed events keep the old
   agent ID and stay queryable; only the live identity, its mesh target,
   and its certificate records are removed. Double-check the ID before
   deleting — removing the *new* row takes that agent's fresh identity
   with it.

6. **Wrap up.** Once the fleet is healthy, the retired directory that
   `ca retire` printed (`ca.retired-<timestamp>`, inside the `server-state`
   volume) can be deleted; keep it until then as the rollback path
   (restore it over `ca/` and re-enroll agents against the old
   fingerprints).
