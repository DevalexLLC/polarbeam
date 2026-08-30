package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/devalexllc/polarbeam/internal/server/probeadmin"
	"github.com/devalexllc/polarbeam/internal/server/probeid"
)

// ErrNotFound marks admin lookups that resolved nothing; httpapi maps it to
// 404 without string matching. Match with errors.Is.
var ErrNotFound = errors.New("not found")

// notFoundError keeps each site's exact human-readable message (the CLI
// prints these verbatim) while still matching errors.Is(err, ErrNotFound).
type notFoundError struct{ msg string }

func (e notFoundError) Error() string        { return e.msg }
func (e notFoundError) Is(target error) bool { return target == ErrNotFound }

func notFoundf(format string, args ...any) error {
	return notFoundError{msg: fmt.Sprintf(format, args...)}
}

// ErrInvalid marks admin writes that can never succeed as requested;
// httpapi maps it to 400.
var ErrInvalid = errors.New("invalid")

type invalidError struct{ msg string }

func (e invalidError) Error() string        { return e.msg }
func (e invalidError) Is(target error) bool { return target == ErrInvalid }

func invalidf(format string, args ...any) error {
	return invalidError{msg: fmt.Sprintf(format, args...)}
}

// ErrConflict marks admin writes refused because they collide with an
// existing row of a different kind; httpapi maps it to 409.
var ErrConflict = errors.New("conflict")

type conflictError struct{ msg string }

func (e conflictError) Error() string        { return e.msg }
func (e conflictError) Is(target error) bool { return target == ErrConflict }

func conflictf(format string, args ...any) error {
	return conflictError{msg: fmt.Sprintf(format, args...)}
}

// isFKViolation reports whether err is a PostgreSQL foreign-key violation
// (SQLSTATE 23503). Admin writers resolve names to ids before inserting, so
// a parent row deleted in between (possible since sites became deletable)
// surfaces here — callers translate it to a typed error instead of letting
// it escape as an opaque 500.
func isFKViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// InUseError reports a target delete blocked by referencing probe configs;
// httpapi maps it to 409.
type InUseError struct {
	Name  string
	Count int64
}

func (e InUseError) Error() string {
	return fmt.Sprintf("target %q is referenced by %d probe config(s)", e.Name, e.Count)
}

// TargetInfo is a targets row as shown by the admin CLI.
type TargetInfo struct {
	ID      uuid.UUID
	Kind    string
	Name    string
	AgentID *uuid.UUID
	Address string
	Port    int32
	URL     string
	// Network is the owning plane's name, "" for a global (operator-owned)
	// external target. Agent targets never carry one — their plane is the
	// agent's, and targets_network_external_check makes that structural.
	Network    string
	ProbeCount int64
	CreatedAt  time.Time
}

// MeshGroupInfo is a mesh group with its member site names.
type MeshGroupInfo struct {
	ID         uuid.UUID
	Name       string
	Network    string
	Sites      []string
	ProbeCount int64
}

// ProbeConfigInfo is a probe_configs row as shown by the admin CLI. Exactly
// one of Site/Target (direct) or Mesh (template) is set.
type ProbeConfigInfo struct {
	ID           uuid.UUID
	Site         string
	Target       string
	Mesh         string
	Network      string // direct rows: own network; mesh rows: the mesh's
	ProbeType    int16
	Interval     time.Duration
	Timeout      time.Duration
	TrainCount   int32
	TrainSpacing time.Duration
	Params       map[string]string
	Enabled      bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
	UpdatedBy    string
}

// ProbeSettings are the type/cadence knobs shared by direct and mesh probes.
type ProbeSettings struct {
	ProbeType    int16
	Interval     time.Duration
	Timeout      time.Duration
	TrainCount   int32
	TrainSpacing time.Duration
	Params       map[string]string
}

// UpsertExternalTarget creates or updates an external target by name and
// returns its ID. Idempotent so dev bootstrap can re-run safely.
//
// networkID is the owning plane: nil means global (operator-owned, readable
// everywhere, writable only by a global admin), set means tenant-owned.
// scope is the caller's network scope (nil = unscoped): a scoped caller may
// only update rows already on one of its planes, so re-upserting a global
// or a co-tenant's target is ErrNotFound, byte-identical to a name that
// does not exist. Ownership itself is immutable — moving a target between
// planes would retarget every probe pointing at it — so an upsert naming a
// different network than the stored row conflicts.
func (s *Store) UpsertExternalTarget(ctx context.Context, name, address string, port int32, url string, networkID *uuid.UUID, scope []uuid.UUID) (uuid.UUID, error) {
	// Any change here can alter probe expansion or expected pairs.
	defer s.noteConfigWrite(ctx)
	// One statement, so a concurrent create of the same name resolves in the
	// index rather than between a SELECT and an INSERT. The DO UPDATE's WHERE
	// carries the three rules an update must satisfy; when it rejects the
	// row, RETURNING yields nothing and the row is re-read below to say WHICH
	// rule it broke. (A read-modify-write here would 500 on the unique
	// violation instead — the shape this replaced.)
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO targets (kind, name, address, port, url, network_id)
		VALUES ('external', $1, $2, $3, $4, $5)
		ON CONFLICT (name) DO UPDATE
			SET address = EXCLUDED.address, port = EXCLUDED.port, url = EXCLUDED.url
			WHERE targets.kind = 'external'
			  AND ($6::uuid[] IS NULL
			       OR (targets.network_id IS NOT NULL AND targets.network_id = ANY($6)))
			  AND ($5::uuid IS NULL OR targets.network_id = $5)
		RETURNING id`, name, address, port, url, networkID, scope).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("upsert target %q: %w", name, err)
	}

	// The conflicting row exists but the update was refused. Re-read it to
	// name the reason; a row that vanished in between is reported as the
	// conflict it was, not retried into a loop.
	var (
		kind    string
		network *uuid.UUID
	)
	err = s.pool.QueryRow(ctx, `
		SELECT kind, network_id FROM targets WHERE name = $1`, name).Scan(&kind, &network)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, conflictf("target %q was created and removed concurrently; retry", name)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("upsert target %q: %w", name, err)
	}
	switch {
	case kind != "external":
		return uuid.Nil, conflictf("target %q already exists as an agent target", name)
	case !targetInScope(network, scope):
		// A global target, or a co-tenant's: ErrNotFound, byte-identical to
		// the name never having existed.
		return uuid.Nil, notFoundf("external target %q does not exist", name)
	default:
		// In scope but bound to a different plane. Ownership is immutable —
		// moving a target would retarget every probe pointing at it.
		return uuid.Nil, conflictf("target %q already exists on a different network; delete and re-create it to move planes", name)
	}
}

// targetInScope reports whether a caller with the given network scope may
// WRITE a target owned by network (nil = global). Unscoped callers may
// write anything; scoped callers may write only their own planes' rows —
// global targets stay operator property.
func targetInScope(network *uuid.UUID, scope []uuid.UUID) bool {
	if scope == nil {
		return true
	}
	return network != nil && slices.Contains(scope, *network)
}

// targetVisible reports whether a caller may SEE and point probes at a
// target owned by network. Deliberately wider than targetInScope: a global
// target is published for every plane to probe, it just cannot be edited by
// them. Mirrors ListTargets' external-target predicate.
func targetVisible(network *uuid.UUID, scope []uuid.UUID) bool {
	return scope == nil || network == nil || slices.Contains(scope, *network)
}

// ListTargets returns all targets, agents included, each with the number of
// probe configs referencing it (the UI blocks deletes while in use).
// networks is the caller's network scope (nil = unfiltered): agent-kind
// targets are kept only when the owning agent sits on an allowed plane;
// external targets are visible when they are global (network_id IS NULL —
// operator-published destinations every plane may probe) or owned by an
// allowed plane. The probe count is scoped too — an
// external target shared by several tenants would otherwise report the
// server-wide count, leaking other tenants' probe activity and
// contradicting the caller's own scoped probe list. Only DIRECT rows
// reference a target (the probe_configs CHECK keeps mesh templates'
// target_id NULL), and those always carry their own network_id, so the
// predicate needs no mesh join.
func (s *Store) ListTargets(ctx context.Context, networks []uuid.UUID) ([]TargetInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT targets.id, targets.kind, targets.name, targets.agent_id,
		       targets.address, targets.port, targets.url, targets.created_at,
		       coalesce(n.name, ''),
		       (SELECT count(*) FROM probe_configs pc
		         WHERE pc.target_id = targets.id
		           AND ($1::uuid[] IS NULL OR pc.network_id = ANY($1)))
		FROM targets
		LEFT JOIN networks n ON n.id = targets.network_id
		WHERE $1::uuid[] IS NULL
		   OR (targets.agent_id IS NULL
		       AND (targets.network_id IS NULL OR targets.network_id = ANY($1)))
		   OR EXISTS (SELECT 1 FROM agents a WHERE a.id = targets.agent_id AND a.network_id = ANY($1))
		ORDER BY targets.kind, targets.name`, networks)
	if err != nil {
		return nil, fmt.Errorf("list targets: %w", err)
	}
	defer rows.Close()
	var out []TargetInfo
	for rows.Next() {
		var t TargetInfo
		if err := rows.Scan(&t.ID, &t.Kind, &t.Name, &t.AgentID, &t.Address, &t.Port, &t.URL,
			&t.CreatedAt, &t.Network, &t.ProbeCount); err != nil {
			return nil, fmt.Errorf("list targets: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteTarget removes an external target by name. Agent targets cannot be
// deleted (they go away with the agent); a target referenced by probe
// configs returns InUseError naming the count. The FOR UPDATE lock blocks a
// concurrent probe add from committing a new reference mid-delete (FK
// checks take a key-share lock, which conflicts).
//
// scope is the caller's network scope (nil = unscoped). A scoped caller may
// delete only its own planes' targets; a global target or a co-tenant's is
// ErrNotFound, indistinguishable from a name that never existed.
func (s *Store) DeleteTarget(ctx context.Context, name string, scope []uuid.UUID) error {
	// Any change here can alter probe expansion or expected pairs.
	defer s.noteConfigWrite(ctx)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("delete target %q: %w", name, err)
	}
	defer tx.Rollback(ctx)

	var (
		id      uuid.UUID
		network *uuid.UUID
	)
	err = tx.QueryRow(ctx, `
		SELECT id, network_id FROM targets
		 WHERE name = $1 AND kind = 'external' FOR UPDATE`, name).Scan(&id, &network)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFoundf("external target %q does not exist", name)
	}
	if err != nil {
		return fmt.Errorf("delete target %q: %w", name, err)
	}
	if !targetInScope(network, scope) {
		return notFoundf("external target %q does not exist", name)
	}
	var inUse int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM probe_configs WHERE target_id = $1`, id).Scan(&inUse); err != nil {
		return fmt.Errorf("delete target %q: %w", name, err)
	}
	if inUse > 0 {
		return InUseError{Name: name, Count: inUse}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM targets WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete target %q: %w", name, err)
	}
	return tx.Commit(ctx)
}

// UpsertMeshGroup creates a mesh group if it does not exist and returns its
// ID. A nil networkID expresses no opinion: new meshes land on the default
// network and re-upserting an existing mesh keeps its network — plain
// `mesh create` stays idempotent for scripts. A non-nil networkID is an
// explicit claim: it binds a new mesh, is an idempotent no-op when it
// matches the existing row, and conflicts when it differs — a mesh's
// network is immutable (moving it would silently retarget every expanded
// series), so the mesh must be deleted and re-created to change planes.
func (s *Store) UpsertMeshGroup(ctx context.Context, name string, networkID *uuid.UUID) (uuid.UUID, error) {
	// Any change here can alter probe expansion or expected pairs.
	defer s.noteConfigWrite(ctx)
	netID := networkID
	if netID == nil {
		id, err := s.NetworkIDByName(ctx, "default")
		if err != nil {
			return uuid.Nil, fmt.Errorf("upsert mesh group %q: %w", name, err)
		}
		netID = &id
	}
	var (
		id     uuid.UUID
		gotNet uuid.UUID
	)
	err := s.pool.QueryRow(ctx, `
		INSERT INTO mesh_groups (name, network_id) VALUES ($1, $2)
		ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
		RETURNING id, network_id`, name, *netID).Scan(&id, &gotNet)
	if isFKViolation(err) {
		return uuid.Nil, notFoundf("network %s no longer exists", *netID)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("upsert mesh group %q: %w", name, err)
	}
	if networkID != nil && gotNet != *networkID {
		return uuid.Nil, conflictf("mesh %q already exists on another network (a mesh's network cannot be changed; delete and re-create it)", name)
	}
	return id, nil
}

// SiteIDByName resolves a site name WITHOUT creating it — admin commands
// against a typo'd site must fail loudly, unlike token creation.
func (s *Store) SiteIDByName(ctx context.Context, name string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT id FROM sites WHERE name = $1`, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, notFoundf("site %q does not exist", name)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve site %q: %w", name, err)
	}
	return id, nil
}

func (s *Store) meshIDByName(ctx context.Context, name string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT id FROM mesh_groups WHERE name = $1`, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, notFoundf("mesh group %q does not exist", name)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve mesh group %q: %w", name, err)
	}
	return id, nil
}

// lockMesh takes the mesh group's row lock. Membership mutations, mesh probe
// creation, and mesh deletion all take it FIRST, before touching series rows,
// so the "does this mesh have enough members" decision is serialized and
// every path locks in the same order. Without the shared order a member
// removal (mesh row, then series) racing a mesh delete (series, then mesh
// row) deadlocks and Postgres aborts one at random.
//
// FOR NO KEY UPDATE, not FOR UPDATE: this only needs to exclude the other
// callers of lockMesh. FOR UPDATE would additionally conflict with the
// FOR KEY SHARE that any insert or delete on probe_configs / mesh_members
// takes on its parent mesh row, which reintroduces the same inversion
// against paths that never call lockMesh at all (deleting a probe config
// cleans series rows, then touches the parent row via its foreign key).
// The name is carried only for the error text: meshIDByName resolves outside
// this transaction, so a mesh deleted in the gap is gone by the time the lock
// is taken. That is a not-found, not an internal error — every caller must
// answer 404 for it, exactly as an already-missing mesh does.
// lockMesh takes the mesh row lock and, for a scoped caller, proves the mesh
// is on one of its planes. scope nil = unscoped.
//
// The scope check belongs HERE, under the lock, not in the handler. Mesh
// names are globally unique but reusable: an httpapi-side name→network
// lookup could be invalidated by a delete-and-recreate on another plane
// before the store re-resolved the same name, and the scoped caller would
// then mutate a mesh it never owned. Checking the locked row closes that
// window by construction — after this returns, the row cannot change until
// the transaction ends.
//
// Out of scope is ErrNotFound with the same wording a missing mesh gets, so
// the two are indistinguishable.
func lockMesh(ctx context.Context, tx pgx.Tx, meshID uuid.UUID, name string, scope []uuid.UUID) error {
	var networkID uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT network_id FROM mesh_groups WHERE id = $1 FOR NO KEY UPDATE`, meshID).Scan(&networkID)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFoundf("mesh group %q does not exist", name)
	}
	if err != nil {
		return fmt.Errorf("lock mesh group %q: %w", name, err)
	}
	if scope != nil && !slices.Contains(scope, networkID) {
		return notFoundf("mesh group %q does not exist", name)
	}
	return nil
}

// AddMeshMember adds a site to a mesh group. Idempotent.
func (s *Store) AddMeshMember(ctx context.Context, meshName, siteName string, scope []uuid.UUID) error {
	// Any change here can alter probe expansion or expected pairs.
	defer s.noteConfigWrite(ctx)
	meshID, err := s.meshIDByName(ctx, meshName)
	if err != nil {
		return err
	}
	siteID, err := s.SiteIDByName(ctx, siteName)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("add %q to mesh %q: %w", siteName, meshName, err)
	}
	defer tx.Rollback(ctx)
	if err := lockMesh(ctx, tx, meshID, meshName, scope); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO mesh_members (mesh_id, site_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, meshID, siteID)
	if isFKViolation(err) {
		// The site vanished between resolution and insert (the mesh cannot:
		// lockMesh holds it). Concurrent site delete — a 404, not a 500.
		return notFoundf("site %q no longer exists", siteName)
	}
	if err != nil {
		return fmt.Errorf("add %q to mesh %q: %w", siteName, meshName, err)
	}
	return tx.Commit(ctx)
}

// RemoveMeshMember removes a site from a mesh group and cleans up the
// series of every expanded probe involving that site (both directions, per
// template) — those series stop producing results, so their open incidents
// would otherwise stay active forever.
func (s *Store) RemoveMeshMember(ctx context.Context, meshName, siteName string, scope []uuid.UUID) error {
	// Any change here can alter probe expansion or expected pairs.
	defer s.noteConfigWrite(ctx)
	meshID, err := s.meshIDByName(ctx, meshName)
	if err != nil {
		return err
	}
	siteID, err := s.SiteIDByName(ctx, siteName)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("remove %q from mesh %q: %w", siteName, meshName, err)
	}
	defer tx.Rollback(ctx)
	if err := lockMesh(ctx, tx, meshID, meshName, scope); err != nil {
		return err
	}

	templates, err := meshTemplateIDs(ctx, tx, meshID)
	if err != nil {
		return fmt.Errorf("remove %q from mesh %q: %w", siteName, meshName, err)
	}
	members, err := meshMemberSiteIDs(ctx, tx, meshID)
	if err != nil {
		return fmt.Errorf("remove %q from mesh %q: %w", siteName, meshName, err)
	}
	// Removing a member is the other way to reach a mesh that cannot expand.
	// Only guard an actual member: a site that is not in the mesh changes no
	// count and must still get the not-found answer below.
	if slices.Contains(members, siteID) {
		problems := probeadmin.ValidateMeshMemberRemoval(meshName, len(members)-1, len(templates))
		if len(problems) > 0 {
			return conflictf("%s", strings.Join(problems, "; "))
		}
	}
	targets, err := meshMemberTargets(ctx, tx, meshID)
	if err != nil {
		return fmt.Errorf("remove %q from mesh %q: %w", siteName, meshName, err)
	}
	// The removed site's outbound series (it as source, every other member's
	// agents as targets) and inbound series (every other member as source,
	// its agents as targets).
	var ids []uuid.UUID
	for _, tmpl := range templates {
		for _, other := range members {
			if other == siteID {
				continue
			}
			for _, target := range targets[other] {
				ids = append(ids, probeid.MeshProbeID(tmpl, siteID, target))
			}
			for _, target := range targets[siteID] {
				ids = append(ids, probeid.MeshProbeID(tmpl, other, target))
			}
		}
	}
	if err := cleanupSeries(ctx, tx, ids, true); err != nil {
		return fmt.Errorf("remove %q from mesh %q: %w", siteName, meshName, err)
	}

	tag, err := tx.Exec(ctx, `
		DELETE FROM mesh_members WHERE mesh_id = $1 AND site_id = $2`, meshID, siteID)
	if err != nil {
		return fmt.Errorf("remove %q from mesh %q: %w", siteName, meshName, err)
	}
	if tag.RowsAffected() == 0 {
		return notFoundf("site %q is not a member of mesh %q", siteName, meshName)
	}
	return tx.Commit(ctx)
}

// ListMeshGroups returns all mesh groups with their member site names and
// the number of probe templates on each (the UI surfaces delete blast
// radius). networks is the caller's network scope (nil = unfiltered).
func (s *Store) ListMeshGroups(ctx context.Context, networks []uuid.UUID) ([]MeshGroupInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT g.id, g.name, n.name, COALESCE(array_agg(s.name ORDER BY s.name) FILTER (WHERE s.name IS NOT NULL), '{}'),
		       (SELECT count(*) FROM probe_configs pc WHERE pc.mesh_id = g.id)
		FROM mesh_groups g
		JOIN networks n ON n.id = g.network_id
		LEFT JOIN mesh_members m ON m.mesh_id = g.id
		LEFT JOIN sites s ON s.id = m.site_id
		WHERE $1::uuid[] IS NULL OR g.network_id = ANY($1)
		GROUP BY g.id, g.name, n.name ORDER BY g.name`, networks)
	if err != nil {
		return nil, fmt.Errorf("list mesh groups: %w", err)
	}
	defer rows.Close()
	var out []MeshGroupInfo
	for rows.Next() {
		var g MeshGroupInfo
		if err := rows.Scan(&g.ID, &g.Name, &g.Network, &g.Sites, &g.ProbeCount); err != nil {
			return nil, fmt.Errorf("list mesh groups: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// DeleteMeshGroup removes a mesh group; the FK cascades its memberships and
// probe templates. Every expanded series is cleaned up first so no open
// incident outlives its probe. Returns how many probe templates went with
// the mesh so callers can surface the blast radius.
func (s *Store) DeleteMeshGroup(ctx context.Context, name string, scope []uuid.UUID) (int64, error) {
	// Any change here can alter probe expansion or expected pairs.
	defer s.noteConfigWrite(ctx)
	meshID, err := s.meshIDByName(ctx, name)
	if err != nil {
		return 0, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("delete mesh %q: %w", name, err)
	}
	defer tx.Rollback(ctx)
	// Before any series row, matching RemoveMeshMember and AddMeshProbe —
	// cleaning up first and letting the DELETE take the mesh row last would
	// invert the order and deadlock against them.
	if err := lockMesh(ctx, tx, meshID, name, scope); err != nil {
		return 0, err
	}

	templates, err := meshTemplateIDs(ctx, tx, meshID)
	if err != nil {
		return 0, fmt.Errorf("delete mesh %q: %w", name, err)
	}
	members, err := meshMemberSiteIDs(ctx, tx, meshID)
	if err != nil {
		return 0, fmt.Errorf("delete mesh %q: %w", name, err)
	}
	targets, err := meshMemberTargets(ctx, tx, meshID)
	if err != nil {
		return 0, fmt.Errorf("delete mesh %q: %w", name, err)
	}
	if err := cleanupSeries(ctx, tx, expandMeshProbeIDs(templates, members, targets), true); err != nil {
		return 0, fmt.Errorf("delete mesh %q: %w", name, err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM mesh_groups WHERE id = $1`, meshID); err != nil {
		return 0, fmt.Errorf("delete mesh %q: %w", name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("delete mesh %q: %w", name, err)
	}
	return int64(len(templates)), nil
}

// AddDirectProbe assigns a probe of target to every agent at site on the
// given network. Callers resolve the network name (empty input means
// 'default') via NetworkIDByName. Only external targets are accepted: an
// agent-kind target row carries no address/port/URL (mesh expansion
// resolves peers via probe_address), so a direct probe against one would
// fail on an empty destination every run.
//
// scope is the caller's network scope (nil = unscoped) and bounds which
// TARGET may be pointed at — a separate question from which network the
// probe lands on, which the caller proved before resolving networkID.
// Global targets stay probeable by everyone (that is what publishing one
// means); a co-tenant's is ErrNotFound, exactly as an unknown name is.
//
// Without this a tenant could attach a probe to another's target row. The
// victim's own probe_count is scope-filtered, so the row would be invisible
// to them while DeleteTarget's unscoped count still refused the delete —
// one tenant pinning another's target undeletable, with no way to see why.
func (s *Store) AddDirectProbe(ctx context.Context, siteName, targetName string, networkID uuid.UUID, ps ProbeSettings, enabled bool, updatedBy string, scope []uuid.UUID) (uuid.UUID, error) {
	// Any change here can alter probe expansion or expected pairs.
	defer s.noteConfigWrite(ctx)
	siteID, err := s.SiteIDByName(ctx, siteName)
	if err != nil {
		return uuid.Nil, err
	}
	var (
		targetID      uuid.UUID
		targetKind    string
		targetNetwork *uuid.UUID
	)
	err = s.pool.QueryRow(ctx,
		`SELECT id, kind, network_id FROM targets WHERE name = $1`,
		targetName).Scan(&targetID, &targetKind, &targetNetwork)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, notFoundf("target %q does not exist", targetName)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve target %q: %w", targetName, err)
	}
	if !targetVisible(targetNetwork, scope) {
		// Same sentence a nonexistent name gets: the probe surface must not
		// become an oracle for other tenants' target names.
		return uuid.Nil, notFoundf("target %q does not exist", targetName)
	}
	if targetKind != "external" {
		return uuid.Nil, invalidf("target %q is an enrollment-managed agent target: direct probes need an external target (mesh probes cover agent peers)", targetName)
	}
	var id uuid.UUID
	err = s.pool.QueryRow(ctx, `
		INSERT INTO probe_configs (site_id, target_id, network_id, probe_type, interval_ms, timeout_ms, train_count, train_spacing_ms, params, enabled, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id`,
		siteID, targetID, networkID, ps.ProbeType, ps.Interval.Milliseconds(), ps.Timeout.Milliseconds(),
		ps.TrainCount, ps.TrainSpacing.Milliseconds(), ps.Params, enabled, updatedBy).Scan(&id)
	if isFKViolation(err) {
		// Site, target, or network deleted between resolution and insert — 404.
		return uuid.Nil, notFoundf("site %q, target %q, or network %s no longer exists", siteName, targetName, networkID)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("add probe: %w", err)
	}
	return id, nil
}

// AddMeshProbe creates a mesh probe template, expanded per source site ×
// destination agent. Multiple templates of the same probe type on one mesh
// are allowed and are distinct series (the template row ID namespaces every
// derived probe ID) — useful for differing cadence or params, though the
// matrix/pair views aggregate by (agent, target, probe_type) and so blend
// same-type templates.
func (s *Store) AddMeshProbe(ctx context.Context, meshName string, ps ProbeSettings, enabled bool, updatedBy string, scope []uuid.UUID) (uuid.UUID, error) {
	// Any change here can alter probe expansion or expected pairs.
	defer s.noteConfigWrite(ctx)
	meshID, err := s.meshIDByName(ctx, meshName)
	if err != nil {
		return uuid.Nil, err
	}
	// Count and insert are one locked decision: the mesh row lock is shared
	// with AddMeshMember/RemoveMeshMember, so membership cannot shift under
	// this check, and the refusal reports the exact count it refused on.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("add mesh probe: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockMesh(ctx, tx, meshID, meshName, scope); err != nil {
		return uuid.Nil, err
	}

	var members int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM mesh_members WHERE mesh_id = $1`, meshID).Scan(&members); err != nil {
		return uuid.Nil, fmt.Errorf("add mesh probe: count members: %w", err)
	}
	if problems := probeadmin.ValidateMeshMembers(meshName, members); len(problems) > 0 {
		return uuid.Nil, invalidf("%s", strings.Join(problems, "; "))
	}

	var id uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO probe_configs (mesh_id, probe_type, interval_ms, timeout_ms, train_count, train_spacing_ms, params, enabled, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id`,
		meshID, ps.ProbeType, ps.Interval.Milliseconds(), ps.Timeout.Milliseconds(),
		ps.TrainCount, ps.TrainSpacing.Milliseconds(), ps.Params, enabled, updatedBy).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("add mesh probe: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("add mesh probe: %w", err)
	}
	return id, nil
}

// probeConfigSelect resolves names for both assignment shapes. The network
// comes from the row itself for direct probes and from the mesh group for
// templates; the probe_configs CHECK guarantees exactly one side is set, so
// the COALESCE never falls through to ” for a real row.
const probeConfigSelect = `
	SELECT pc.id, COALESCE(s.name, ''), COALESCE(t.name, ''), COALESCE(g.name, ''),
	       pc.probe_type, pc.interval_ms, pc.timeout_ms, pc.train_count, pc.train_spacing_ms,
	       pc.params, pc.enabled, pc.created_at, pc.updated_at, pc.updated_by,
	       COALESCE(nd.name, ng.name, '')
	FROM probe_configs pc
	LEFT JOIN sites s ON s.id = pc.site_id
	LEFT JOIN targets t ON t.id = pc.target_id
	LEFT JOIN mesh_groups g ON g.id = pc.mesh_id
	LEFT JOIN networks nd ON nd.id = pc.network_id
	LEFT JOIN networks ng ON ng.id = g.network_id`

func scanProbeConfig(row pgx.Row) (ProbeConfigInfo, error) {
	var (
		p                                     ProbeConfigInfo
		intervalMS, timeoutMS, trainSpacingMS int64
	)
	err := row.Scan(&p.ID, &p.Site, &p.Target, &p.Mesh, &p.ProbeType,
		&intervalMS, &timeoutMS, &p.TrainCount, &trainSpacingMS, &p.Params, &p.Enabled,
		&p.CreatedAt, &p.UpdatedAt, &p.UpdatedBy, &p.Network)
	if err != nil {
		return p, err
	}
	p.Interval = time.Duration(intervalMS) * time.Millisecond
	p.Timeout = time.Duration(timeoutMS) * time.Millisecond
	p.TrainSpacing = time.Duration(trainSpacingMS) * time.Millisecond
	return p, nil
}

// ListProbeConfigs returns every probe config with names resolved. networks
// is the caller's network scope (nil = unfiltered): direct rows filter on
// their own plane, mesh templates on the owning mesh's.
func (s *Store) ListProbeConfigs(ctx context.Context, networks []uuid.UUID) ([]ProbeConfigInfo, error) {
	rows, err := s.pool.Query(ctx, probeConfigSelect+`
	WHERE $1::uuid[] IS NULL OR COALESCE(pc.network_id, g.network_id) = ANY($1)
	ORDER BY pc.created_at`, networks)
	if err != nil {
		return nil, fmt.Errorf("list probe configs: %w", err)
	}
	defer rows.Close()
	var out []ProbeConfigInfo
	for rows.Next() {
		p, err := scanProbeConfig(rows)
		if err != nil {
			return nil, fmt.Errorf("list probe configs: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProbeConfig returns one probe config with names resolved.
func (s *Store) GetProbeConfig(ctx context.Context, id uuid.UUID) (*ProbeConfigInfo, error) {
	p, err := scanProbeConfig(s.pool.QueryRow(ctx, probeConfigSelect+` WHERE pc.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFoundf("probe config %s does not exist", id)
	}
	if err != nil {
		return nil, fmt.Errorf("get probe config: %w", err)
	}
	return &p, nil
}

// UpdateProbeConfig edits a probe's cadence/train/params/enabled in place —
// identity (type, assignment) is immutable, enforced by callers, so the
// probe ID and its series history stay continuous. Disabling cleans up the
// expanded series (open incidents close; counters reset) because a disabled
// probe stops producing the results that would ever close them.
func (s *Store) UpdateProbeConfig(ctx context.Context, id uuid.UUID, ps ProbeSettings, enabled bool, updatedBy string) error {
	// Any change here can alter probe expansion or expected pairs.
	defer s.noteConfigWrite(ctx)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("update probe config: %w", err)
	}
	defer tx.Rollback(ctx)

	var (
		meshID     *uuid.UUID
		wasEnabled bool
	)
	err = tx.QueryRow(ctx, `
		SELECT mesh_id, enabled FROM probe_configs WHERE id = $1 FOR UPDATE`,
		id).Scan(&meshID, &wasEnabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFoundf("probe config %s does not exist", id)
	}
	if err != nil {
		return fmt.Errorf("update probe config: %w", err)
	}

	if wasEnabled && !enabled {
		ids, err := expandedProbeIDs(ctx, tx, id, meshID)
		if err != nil {
			return fmt.Errorf("update probe config: %w", err)
		}
		if err := cleanupSeries(ctx, tx, ids, false); err != nil {
			return fmt.Errorf("update probe config: %w", err)
		}
	}

	_, err = tx.Exec(ctx, `
		UPDATE probe_configs
		SET interval_ms = $2, timeout_ms = $3, train_count = $4, train_spacing_ms = $5,
		    params = $6, enabled = $7, updated_at = now(), updated_by = $8
		WHERE id = $1`,
		id, ps.Interval.Milliseconds(), ps.Timeout.Milliseconds(), ps.TrainCount,
		ps.TrainSpacing.Milliseconds(), ps.Params, enabled, updatedBy)
	if err != nil {
		return fmt.Errorf("update probe config: %w", err)
	}
	return tx.Commit(ctx)
}

// DeleteProbeConfig removes a probe config by ID, cleaning up the expanded
// series first (open incidents close; series/traceroute state is deleted).
func (s *Store) DeleteProbeConfig(ctx context.Context, id uuid.UUID) error {
	// Any change here can alter probe expansion or expected pairs.
	defer s.noteConfigWrite(ctx)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("delete probe config: %w", err)
	}
	defer tx.Rollback(ctx)

	var meshID *uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT mesh_id FROM probe_configs WHERE id = $1 FOR UPDATE`,
		id).Scan(&meshID)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFoundf("probe config %s does not exist", id)
	}
	if err != nil {
		return fmt.Errorf("delete probe config: %w", err)
	}

	ids, err := expandedProbeIDs(ctx, tx, id, meshID)
	if err != nil {
		return fmt.Errorf("delete probe config: %w", err)
	}
	if err := cleanupSeries(ctx, tx, ids, true); err != nil {
		return fmt.Errorf("delete probe config: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM probe_configs WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete probe config: %w", err)
	}
	return tx.Commit(ctx)
}

// meshTemplateIDs returns the config-row ID of every template on a mesh —
// expansion is per template row, and the row ID is the namespace of every
// probe ID the template derives.
func meshTemplateIDs(ctx context.Context, tx pgx.Tx, meshID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM probe_configs WHERE mesh_id = $1`, meshID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func meshMemberSiteIDs(ctx context.Context, tx pgx.Tx, meshID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `SELECT site_id FROM mesh_members WHERE mesh_id = $1`, meshID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// meshMemberTargets maps each member site of a mesh to the agent-kind
// target IDs of its agents ON THE MESH'S NETWORK — the destination axis of
// the probe ID derivation. The network predicate here is what keeps every
// re-derivation (cleanup, retirement, expandedProbeIDs) aligned with the
// expansion agents actually run.
func meshMemberTargets(ctx context.Context, tx pgx.Tx, meshID uuid.UUID) (map[uuid.UUID][]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `
		SELECT mm.site_id, t.id
		FROM mesh_members mm
		JOIN mesh_groups mg ON mg.id = mm.mesh_id
		JOIN agents a ON a.site_id = mm.site_id AND a.network_id = mg.network_id
		JOIN targets t ON t.agent_id = a.id
		WHERE mm.mesh_id = $1`, meshID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID][]uuid.UUID)
	for rows.Next() {
		var siteID, targetID uuid.UUID
		if err := rows.Scan(&siteID, &targetID); err != nil {
			return nil, err
		}
		out[siteID] = append(out[siteID], targetID)
	}
	return out, rows.Err()
}

// expandMeshProbeIDs derives the probe ID of every template × source site ×
// destination-site agent target — the same derivation meshexpand uses to
// build snapshots, so cleanup hits exactly the series agents were running.
func expandMeshProbeIDs(templateIDs []uuid.UUID, members []uuid.UUID, siteTargets map[uuid.UUID][]uuid.UUID) []uuid.UUID {
	var ids []uuid.UUID
	for _, tmpl := range templateIDs {
		for _, src := range members {
			for _, dst := range members {
				if src == dst {
					continue
				}
				for _, target := range siteTargets[dst] {
					ids = append(ids, probeid.MeshProbeID(tmpl, src, target))
				}
			}
		}
	}
	return ids
}

// expandedProbeIDs resolves the concrete probe IDs one config row expands
// to: the row's own ID for a direct probe, or the full member expansion for
// a mesh template.
func expandedProbeIDs(ctx context.Context, tx pgx.Tx, id uuid.UUID, meshID *uuid.UUID) ([]uuid.UUID, error) {
	if meshID == nil {
		return []uuid.UUID{id}, nil
	}
	members, err := meshMemberSiteIDs(ctx, tx, *meshID)
	if err != nil {
		return nil, err
	}
	targets, err := meshMemberTargets(ctx, tx, *meshID)
	if err != nil {
		return nil, err
	}
	return expandMeshProbeIDs([]uuid.UUID{id}, members, targets), nil
}

// cleanupSeries closes the open probe_failing or probe_degraded event of
// every listed series — the probe stops producing results, so ingest's
// 3-result close can never happen; closed_at = now() honestly records when
// monitoring was removed.
// deleteRows additionally drops series_state, traceroute_current, and
// path_mtu_current (the probe is gone for good; the pair endpoints select
// current rows by agent/target alone, so a surviving row would be served
// forever); a disable instead resets the hysteresis counters but keeps
// last_time, so a spool replay after re-enable still dedupes against
// out-of-order stragglers.
func cleanupSeries(ctx context.Context, tx pgx.Tx, probeIDs []uuid.UUID, deleteRows bool) error {
	if len(probeIDs) == 0 {
		return nil
	}
	// The kind list matches outage_events_probe_open_kind_idx's predicate
	// verbatim (a broader predicate could not use the partial index).
	if _, err := tx.Exec(ctx, `
		UPDATE outage_events SET closed_at = now()
		WHERE probe_id = ANY($1) AND kind IN ('probe_failing', 'probe_degraded') AND closed_at IS NULL`, probeIDs); err != nil {
		return fmt.Errorf("close open events: %w", err)
	}
	if deleteRows {
		if _, err := tx.Exec(ctx, `DELETE FROM series_state WHERE probe_id = ANY($1)`, probeIDs); err != nil {
			return fmt.Errorf("delete series state: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM traceroute_current WHERE probe_id = ANY($1)`, probeIDs); err != nil {
			return fmt.Errorf("delete traceroute state: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM path_mtu_current WHERE probe_id = ANY($1)`, probeIDs); err != nil {
			return fmt.Errorf("delete path MTU state: %w", err)
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE series_state
		SET open_event_id = NULL, consec_fails = 0, consec_oks = 0,
		    consec_degraded = 0, consec_clean = 0,
		    first_fail_at = NULL, first_ok_at = NULL,
		    first_degraded_at = NULL, first_clean_at = NULL
		WHERE probe_id = ANY($1)`, probeIDs); err != nil {
		return fmt.Errorf("reset series state: %w", err)
	}
	return nil
}

// DirectProbeRow is a direct (site-scoped) probe assignment for one agent.
// DstSiteID is the site of an agent-kind target (nil for external targets);
// ingest uses it to resolve per-pair threshold overrides. It never enters
// the config snapshot, so it cannot perturb config_hash.
type DirectProbeRow struct {
	ID        uuid.UUID
	Settings  ProbeSettings
	TargetID  uuid.UUID
	Kind      string
	Address   string
	Port      int32
	URL       string
	DstSiteID *uuid.UUID
}

// MeshProbeRow is a mesh probe template applying to the agent's site.
// ConfigID is the template's probe_configs.id — the namespace of every
// probe ID the template expands to.
type MeshProbeRow struct {
	ConfigID uuid.UUID
	MeshID   uuid.UUID
	Settings ProbeSettings
}

// PeerRow is a peer agent reachable through a shared mesh.
type PeerRow struct {
	MeshID       uuid.UUID
	AgentID      uuid.UUID
	SiteID       uuid.UUID
	TargetID     uuid.UUID // the peer's agent-kind targets.id
	ProbeAddress string
}

// AgentConfigInputs is everything needed to expand one agent's config
// snapshot (consumed by meshexpand, which is pure).
type AgentConfigInputs struct {
	AgentID uuid.UUID
	SiteID  uuid.UUID
	// NetworkID is the agent's plane. meshexpand never reads it — the SQL
	// below already scopes every row — but ingest needs it to resolve the
	// plane-qualified threshold layers for the pairs this agent measures.
	NetworkID uuid.UUID
	Direct    []DirectProbeRow
	Mesh      []MeshProbeRow
	Peers     []PeerRow
}

// LoadAgentConfigInputs gathers the agent's site, its site's direct probes,
// mesh templates covering the site, and mesh peers — one batched round trip.
// Everything is scoped to the agent's network in the SQL itself (direct
// probes match on (site, network); mesh templates and peers only where the
// mesh's network matches), so meshexpand stays network-ignorant and the
// ingest allowlist tightens for free.
func (s *Store) LoadAgentConfigInputs(ctx context.Context, agentID uuid.UUID) (AgentConfigInputs, error) {
	in := AgentConfigInputs{AgentID: agentID}

	batch := &pgx.Batch{}
	batch.Queue(`SELECT site_id, network_id FROM agents WHERE id = $1`, agentID)
	batch.Queue(`
		SELECT pc.id, pc.probe_type, pc.interval_ms, pc.timeout_ms, pc.train_count, pc.train_spacing_ms, pc.params,
		       t.id, t.kind, t.address, t.port, t.url, dta.site_id
		FROM probe_configs pc
		JOIN targets t ON t.id = pc.target_id
		LEFT JOIN agents dta ON dta.id = t.agent_id
		JOIN agents a ON a.site_id = pc.site_id AND a.network_id = pc.network_id
		WHERE a.id = $1 AND pc.enabled
		ORDER BY pc.created_at`, agentID)
	batch.Queue(`
		SELECT pc.id, pc.mesh_id, pc.probe_type, pc.interval_ms, pc.timeout_ms, pc.train_count, pc.train_spacing_ms, pc.params
		FROM probe_configs pc
		JOIN mesh_groups mg ON mg.id = pc.mesh_id
		JOIN mesh_members mm ON mm.mesh_id = pc.mesh_id
		JOIN agents a ON a.site_id = mm.site_id AND a.network_id = mg.network_id
		WHERE a.id = $1 AND pc.enabled
		ORDER BY pc.created_at`, agentID)
	batch.Queue(`
		SELECT DISTINCT mine.mesh_id, peer.id, peer.site_id, t.id, peer.probe_address
		FROM agents me
		JOIN mesh_members mine ON mine.site_id = me.site_id
		JOIN mesh_groups mg ON mg.id = mine.mesh_id AND mg.network_id = me.network_id
		JOIN mesh_members theirs ON theirs.mesh_id = mine.mesh_id AND theirs.site_id <> mine.site_id
		JOIN agents peer ON peer.site_id = theirs.site_id AND peer.network_id = mg.network_id
		JOIN targets t ON t.agent_id = peer.id
		WHERE me.id = $1
		ORDER BY mine.mesh_id, peer.id, t.id`, agentID)

	res := s.pool.SendBatch(ctx, batch)
	defer res.Close()

	if err := res.QueryRow().Scan(&in.SiteID, &in.NetworkID); err != nil {
		return in, fmt.Errorf("load config inputs: agent %s: %w", agentID, err)
	}

	rows, err := res.Query()
	if err != nil {
		return in, fmt.Errorf("load config inputs: direct probes: %w", err)
	}
	for rows.Next() {
		var (
			d                                     DirectProbeRow
			intervalMS, timeoutMS, trainSpacingMS int64
		)
		if err := rows.Scan(&d.ID, &d.Settings.ProbeType, &intervalMS, &timeoutMS,
			&d.Settings.TrainCount, &trainSpacingMS, &d.Settings.Params,
			&d.TargetID, &d.Kind, &d.Address, &d.Port, &d.URL, &d.DstSiteID); err != nil {
			rows.Close()
			return in, fmt.Errorf("load config inputs: direct probes: %w", err)
		}
		d.Settings.Interval = time.Duration(intervalMS) * time.Millisecond
		d.Settings.Timeout = time.Duration(timeoutMS) * time.Millisecond
		d.Settings.TrainSpacing = time.Duration(trainSpacingMS) * time.Millisecond
		in.Direct = append(in.Direct, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return in, fmt.Errorf("load config inputs: direct probes: %w", err)
	}

	rows, err = res.Query()
	if err != nil {
		return in, fmt.Errorf("load config inputs: mesh probes: %w", err)
	}
	for rows.Next() {
		var (
			m                                     MeshProbeRow
			intervalMS, timeoutMS, trainSpacingMS int64
		)
		if err := rows.Scan(&m.ConfigID, &m.MeshID, &m.Settings.ProbeType, &intervalMS, &timeoutMS,
			&m.Settings.TrainCount, &trainSpacingMS, &m.Settings.Params); err != nil {
			rows.Close()
			return in, fmt.Errorf("load config inputs: mesh probes: %w", err)
		}
		m.Settings.Interval = time.Duration(intervalMS) * time.Millisecond
		m.Settings.Timeout = time.Duration(timeoutMS) * time.Millisecond
		m.Settings.TrainSpacing = time.Duration(trainSpacingMS) * time.Millisecond
		in.Mesh = append(in.Mesh, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return in, fmt.Errorf("load config inputs: mesh probes: %w", err)
	}

	rows, err = res.Query()
	if err != nil {
		return in, fmt.Errorf("load config inputs: peers: %w", err)
	}
	for rows.Next() {
		var p PeerRow
		if err := rows.Scan(&p.MeshID, &p.AgentID, &p.SiteID, &p.TargetID, &p.ProbeAddress); err != nil {
			rows.Close()
			return in, fmt.Errorf("load config inputs: peers: %w", err)
		}
		in.Peers = append(in.Peers, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return in, fmt.Errorf("load config inputs: peers: %w", err)
	}

	return in, nil
}
