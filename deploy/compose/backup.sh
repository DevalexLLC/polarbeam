#!/usr/bin/env bash
# PolarBEAM control-plane backup. Documented in docs/install.md, "Taking a
# backup"; run it as root from the compose directory (it changes into its
# own directory, so a cron entry needs no cd).
#
# Writes one directory, polarbeam-backup-<UTC time>, holding:
#   .env, server.yaml, docker-compose.yml   the installation's configuration
#   db-image.txt                            the database image the dump came from
#   extensions.txt                          TimescaleDB extension versions
#   inventory.txt                           row counts under the dump's own snapshot
#   polarbeam.dump                          pg_dump custom-format archive
#   volumes.tar.gz                          the server-state and tls volumes
#   SHA256SUMS
#
# Fail-loud: the script stops at the first failed command or empty file,
# names the line it stopped on, and prints "backup-ok <dir>" as its last
# line only when every step succeeded. Any other ending is a failed backup
# and the directory it left behind is incomplete.
#
# Environment:
#   POLARBEAM_BACKUP_DIR   where to create the set (default: this directory)
set -euo pipefail
trap 'echo "backup-failed: line $LINENO" >&2' ERR
umask 077
cd "$(dirname "$0")"

# One backup at a time: an overlapping run (a manual backup during the
# scheduled one) would double the load and could interleave the two sets.
exec 9>.backup.lock
flock -n 9 || { echo "backup-failed: another backup is running" >&2; exit 1; }

# The running database container. Its image is taken by ID, not tag: the
# tag is rolling and may already point at a newer image pulled for an
# upgrade, while the ID is the image the dump actually comes from and is
# always present, even on an offline host. The compose label names the
# project, which prefixes the volume names.
DB_CONTAINER=$(docker compose ps -q timescaledb)
[ -n "$DB_CONTAINER" ]
DB_IMAGE=$(docker inspect -f '{{.Image}}' "$DB_CONTAINER")
PROJECT=$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' "$DB_CONTAINER")
[ -n "$PROJECT" ]

BACKUP=${POLARBEAM_BACKUP_DIR:-.}/polarbeam-backup-$(date -u +%Y%m%dT%H%M%SZ)
mkdir "$BACKUP"
for f in .env server.yaml docker-compose.yml; do
  cp "$f" "$BACKUP/"
  [ -s "$BACKUP/$f" ]
done
docker inspect -f '{{.Id}} {{.RepoDigests}} {{.RepoTags}}' "$DB_IMAGE" > "$BACKUP/db-image.txt"
docker compose exec -T timescaledb psql -X -U polarbeam -d polarbeam -Atc \
  "SELECT extname, extversion FROM pg_extension
   WHERE extname IN ('timescaledb', 'timescaledb_toolkit') ORDER BY extname" \
  > "$BACKUP/extensions.txt"
[ -s "$BACKUP/extensions.txt" ]

# One transaction exports a snapshot, counts the inventory under it, and
# hands the same snapshot to pg_dump, so the record describes exactly what
# the dump holds. psql's stdout carries only the dump; the inventory goes
# to a per-run file inside the container and is copied out afterwards.
INV=/tmp/polarbeam-inventory.$$.txt
docker compose exec -T timescaledb psql -X -q -v ON_ERROR_STOP=1 -v inv="$INV" \
  -U polarbeam -d polarbeam > "$BACKUP/polarbeam.dump" <<'SQL'
BEGIN ISOLATION LEVEL REPEATABLE READ;
SELECT pg_export_snapshot() AS snap \gset
\setenv PGSNAP :snap
\pset format unaligned
\pset tuples_only on
\o :inv
SELECT 'sites', count(*) FROM sites
UNION ALL SELECT 'agents', count(*) FROM agents
UNION ALL SELECT 'users', count(*) FROM users
UNION ALL SELECT 'schema_migrations', count(*) FROM schema_migrations
UNION ALL SELECT 'hypertables', count(*) FROM timescaledb_information.hypertables
UNION ALL SELECT 'caggs', count(*) FROM timescaledb_information.continuous_aggregates
UNION ALL SELECT 'probe_results', count(*) FROM probe_results;
\o
\! pg_dump -U polarbeam -Fc --snapshot="$PGSNAP" polarbeam
\if :SHELL_ERROR
  DO $$ BEGIN RAISE EXCEPTION 'pg_dump failed'; END $$;
\endif
COMMIT;
SQL
docker compose exec -T timescaledb cat "$INV" > "$BACKUP/inventory.txt"
docker compose exec -T timescaledb rm "$INV"
[ -s "$BACKUP/inventory.txt" ]
docker run --rm -i "$DB_IMAGE" pg_restore --list < "$BACKUP/polarbeam.dump" > /dev/null

# The two small volumes, archived through the database image (it carries
# tar; the PolarBEAM images are distroless). A volume name that does not
# exist would be mounted as a new empty one without complaint, which is
# what the member check catches.
docker run --rm --user 0 --entrypoint tar \
  -v "${PROJECT}_server-state:/server-state:ro" -v "${PROJECT}_tls:/tls:ro" \
  "$DB_IMAGE" -C / -czf - server-state tls > "$BACKUP/volumes.tar.gz"
# The listing is a separately checked command so a corrupt archive fails
# here rather than being masked inside the comparison.
MEMBERS=$(tar -tzf "$BACKUP/volumes.tar.gz")
[ "$(printf '%s\n' "$MEMBERS" | grep -cx -e server-state/ca/ca.key -e tls/server.key)" = 2 ]

(cd "$BACKUP" && sha256sum .env server.yaml docker-compose.yml db-image.txt extensions.txt \
   inventory.txt polarbeam.dump volumes.tar.gz > SHA256SUMS)
cat "$BACKUP/extensions.txt" "$BACKUP/inventory.txt"
echo "backup-ok $BACKUP"
