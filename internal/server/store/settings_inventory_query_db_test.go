package store_test

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/probeadmin"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

type settingsInventoryFixture struct {
	netFixture
	defaultOnlyAgent uuid.UUID
	serviceTarget    uuid.UUID
	mgmtDirect       uuid.UUID
	mgmtMesh         uuid.UUID
	createdAt        time.Time
}

func seedSettingsInventory(t *testing.T, ctx context.Context, s *store.Store) settingsInventoryFixture {
	t.Helper()
	f := settingsInventoryFixture{netFixture: buildNetFixture(t, ctx, s)}
	f.defaultOnlyAgent = enrollNetAgent(t, ctx, s, "site-default-only", "only-default", nil)
	if err := s.Pool().QueryRow(ctx, `SELECT id FROM targets WHERE name = 'svc'`).Scan(&f.serviceTarget); err != nil {
		t.Fatalf("find service target: %v", err)
	}
	tcp := netProbeSettings
	tcp.ProbeType = 2
	var err error
	f.mgmtDirect, err = s.AddDirectProbe(ctx, "site-b", "svc", f.mgmt, tcp, false, "seed", nil)
	if err != nil {
		t.Fatalf("add mgmt direct probe: %v", err)
	}
	if _, err := s.UpsertMeshGroup(ctx, "m-mgmt", &f.mgmt); err != nil {
		t.Fatalf("create mgmt mesh: %v", err)
	}
	for _, site := range []string{"site-a", "site-b"} {
		if err := s.AddMeshMember(ctx, "m-mgmt", site, nil); err != nil {
			t.Fatalf("add mgmt mesh member %s: %v", site, err)
		}
	}
	f.mgmtMesh, err = s.AddMeshProbe(ctx, "m-mgmt", netProbeSettings, true, "seed", nil)
	if err != nil {
		t.Fatalf("add mgmt mesh probe: %v", err)
	}
	if _, err := s.Pool().Exec(ctx, `
		UPDATE sites SET display_name = CASE name
		  WHEN 'site-a' THEN 'London edge'
		  WHEN 'site-b' THEN 'New York edge'
		  ELSE 'Default only' END,
		  location = CASE name WHEN 'site-a' THEN 'London, UK' ELSE '' END`); err != nil {
		t.Fatalf("label settings sites: %v", err)
	}
	f.createdAt = time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	for _, table := range []string{"sites", "targets", "probe_configs"} {
		column := "created_at"
		if table == "probe_configs" {
			column = "updated_at"
		}
		if _, err := s.Pool().Exec(ctx, "UPDATE "+table+" SET "+column+" = $1", f.createdAt); err != nil {
			t.Fatalf("tie %s timestamps: %v", table, err)
		}
	}
	return f
}

func siteConfigIDs(rows []store.SiteAdminInfo) []uuid.UUID {
	ids := make([]uuid.UUID, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	return ids
}

func targetConfigIDs(rows []store.TargetInfo) []uuid.UUID {
	ids := make([]uuid.UUID, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	return ids
}

func probeConfigIDs(rows []store.ProbeConfigInfo) []uuid.UUID {
	ids := make([]uuid.UUID, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	return ids
}

func stableCompare(primary int, a, b uuid.UUID, order string) int {
	if primary == 0 {
		primary = strings.Compare(a.String(), b.String())
	}
	if order == "desc" {
		return -primary
	}
	return primary
}

func TestSettingsSiteInventoryQueryAndDetail(t *testing.T) {
	ctx, s := newStore(t)
	f := seedSettingsInventory(t, ctx, s)
	query := func(filter store.SiteConfigFilter) ([]store.SiteAdminInfo, int64) {
		t.Helper()
		rows, total, err := s.QuerySitesConfig(ctx, filter)
		if err != nil {
			t.Fatalf("QuerySitesConfig(%+v): %v", filter, err)
		}
		return rows, total
	}
	base := store.SiteConfigFilter{Sort: "name", Order: "asc", Limit: 100}
	rows, total := query(base)
	if len(rows) != 3 || total != 3 {
		t.Fatalf("site inventory = %d rows total %d", len(rows), total)
	}
	for _, sortName := range []string{"name", "display_name", "created", "agents", "meshes", "probes"} {
		for _, order := range []string{"asc", "desc"} {
			filter := base
			filter.Sort, filter.Order = sortName, order
			got, gotTotal := query(filter)
			if gotTotal != 3 || !slices.IsSortedFunc(got, func(a, b store.SiteAdminInfo) int {
				var primary int
				switch sortName {
				case "name":
					primary = strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
				case "display_name":
					primary = strings.Compare(strings.ToLower(a.DisplayName), strings.ToLower(b.DisplayName))
				case "created":
					primary = a.CreatedAt.Compare(b.CreatedAt)
				case "agents":
					primary = cmp.Compare(a.AgentCount, b.AgentCount)
				case "meshes":
					primary = cmp.Compare(a.MeshCount, b.MeshCount)
				case "probes":
					primary = cmp.Compare(a.ProbeCount, b.ProbeCount)
				}
				return stableCompare(primary, a.ID, b.ID, order)
			}) {
				t.Errorf("site sort %s %s unstable: %v", sortName, order, siteConfigIDs(got))
			}
		}
	}

	filter := base
	filter.Query = "London"
	rows, total = query(filter)
	if total != 1 || len(rows) != 1 || rows[0].Name != "site-a" {
		t.Errorf("site display/location search = %v total %d", siteConfigIDs(rows), total)
	}
	filter.Query = "site%_"
	if rows, total = query(filter); len(rows) != 0 || total != 0 {
		t.Errorf("literal site wildcard = %v total %d", siteConfigIDs(rows), total)
	}
	filter = base
	filter.Networks = []uuid.UUID{f.mgmt}
	rows, total = query(filter)
	if total != 2 {
		t.Fatalf("mgmt sites total = %d, rows %+v", total, rows)
	}
	for _, site := range rows {
		if site.Name == "site-a" && (site.AgentCount != 1 || site.MeshCount != 1 || site.ProbeCount != 0) {
			t.Errorf("site-a scoped counts leaked: %+v", site)
		}
	}
	detail, err := s.GetSiteConfig(ctx, "site-a", []uuid.UUID{f.mgmt})
	if err != nil || detail.AgentCount != 1 || detail.MeshCount != 1 || detail.ProbeCount != 0 {
		t.Errorf("scoped site detail = %+v err %v", detail, err)
	}
	if _, err := s.GetSiteConfig(ctx, "site-default-only", []uuid.UUID{f.mgmt}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("inaccessible site detail err = %v, want ErrNotFound", err)
	}

	filter = base
	filter.Sort, filter.Limit = "created", 1
	var paged []uuid.UUID
	for offset := range 3 {
		filter.Offset = offset
		page, pageTotal := query(filter)
		if len(page) != 1 || pageTotal != 3 {
			t.Fatalf("site page %d = %v total %d", offset, page, pageTotal)
		}
		paged = append(paged, page[0].ID)
	}
	if want := sortedUUIDs(paged...); !slices.Equal(paged, want) {
		t.Errorf("stable site pages = %v, want %v", paged, want)
	}
	filter.Offset = 4
	if page, pageTotal := query(filter); len(page) != 0 || pageTotal != 3 {
		t.Errorf("past-end site page = %v total %d", page, pageTotal)
	}

	for _, bad := range []store.SiteConfigFilter{
		{Sort: "bogus", Order: "asc", Limit: 1},
		{Sort: "name", Order: "sideways", Limit: 1},
		{Sort: "name", Order: "asc", Limit: 0},
	} {
		if _, _, err := s.QuerySitesConfig(ctx, bad); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("bad site filter %+v: %v", bad, err)
		}
	}
}

func TestSettingsTargetInventoryQueryAndDetail(t *testing.T) {
	ctx, s := newStore(t)
	f := seedSettingsInventory(t, ctx, s)
	query := func(filter store.TargetConfigFilter) ([]store.TargetInfo, int64) {
		t.Helper()
		rows, total, err := s.QueryTargetsConfig(ctx, filter)
		if err != nil {
			t.Fatalf("QueryTargetsConfig(%+v): %v", filter, err)
		}
		return rows, total
	}
	base := store.TargetConfigFilter{Sort: "name", Order: "asc", Limit: 100}
	rows, total := query(base)
	if len(rows) != 6 || total != 6 {
		t.Fatalf("target inventory = %d rows total %d", len(rows), total)
	}
	for _, sortName := range []string{"name", "kind", "network", "probes", "created"} {
		for _, order := range []string{"asc", "desc"} {
			filter := base
			filter.Sort, filter.Order = sortName, order
			got, gotTotal := query(filter)
			if gotTotal != 6 || !slices.IsSortedFunc(got, func(a, b store.TargetInfo) int {
				var primary int
				switch sortName {
				case "name":
					primary = strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
				case "kind":
					primary = strings.Compare(a.Kind, b.Kind)
				case "network":
					primary = strings.Compare(strings.ToLower(a.Network), strings.ToLower(b.Network))
				case "probes":
					primary = cmp.Compare(a.ProbeCount, b.ProbeCount)
				case "created":
					primary = a.CreatedAt.Compare(b.CreatedAt)
				}
				return stableCompare(primary, a.ID, b.ID, order)
			}) {
				t.Errorf("target sort %s %s unstable: %v", sortName, order, targetConfigIDs(got))
			}
		}
	}

	filter := base
	filter.Query, filter.Kind = "203.0.113.7", "external"
	rows, total = query(filter)
	if total != 1 || len(rows) != 1 || rows[0].ID != f.serviceTarget {
		t.Errorf("target endpoint/kind filter = %v total %d", targetConfigIDs(rows), total)
	}
	filter.Query = "443"
	if rows, total = query(filter); total != 1 || rows[0].ID != f.serviceTarget {
		t.Errorf("target port search = %v total %d", targetConfigIDs(rows), total)
	}
	filter.Query = "svc%_"
	if rows, total = query(filter); len(rows) != 0 || total != 0 {
		t.Errorf("literal target wildcard = %v total %d", targetConfigIDs(rows), total)
	}
	filter = base
	filter.Networks = []uuid.UUID{f.mgmt}
	rows, total = query(filter)
	if total != 3 || slices.Contains(targetConfigIDs(rows), f.tADef) {
		t.Errorf("mgmt targets = %v total %d", targetConfigIDs(rows), total)
	}
	for _, target := range rows {
		if target.ID == f.serviceTarget && target.ProbeCount != 1 {
			t.Errorf("global target scoped count = %+v", target)
		}
	}
	detail, err := s.GetTargetConfig(ctx, "svc", []uuid.UUID{f.mgmt})
	if err != nil || detail.ID != f.serviceTarget || detail.ProbeCount != 1 {
		t.Errorf("scoped target detail = %+v err %v", detail, err)
	}
	defaultName := "agent:" + f.aDef.String()
	if _, err := s.GetTargetConfig(ctx, defaultName, []uuid.UUID{f.mgmt}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("inaccessible target detail err = %v, want ErrNotFound", err)
	}

	filter = base
	filter.Sort, filter.Limit = "created", 2
	var paged []uuid.UUID
	for offset := 0; offset < 6; offset += 2 {
		filter.Offset = offset
		page, pageTotal := query(filter)
		if pageTotal != 6 {
			t.Fatalf("target page %d total = %d", offset, pageTotal)
		}
		paged = append(paged, targetConfigIDs(page)...)
	}
	if want := sortedUUIDs(paged...); !slices.Equal(paged, want) {
		t.Errorf("stable target pages = %v, want %v", paged, want)
	}

	for _, bad := range []store.TargetConfigFilter{
		{Sort: "bogus", Order: "asc", Limit: 1},
		{Sort: "name", Order: "sideways", Limit: 1},
		{Sort: "name", Order: "asc", Kind: "bogus", Limit: 1},
		{Sort: "name", Order: "asc", Limit: 101},
	} {
		if _, _, err := s.QueryTargetsConfig(ctx, bad); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("bad target filter %+v: %v", bad, err)
		}
	}
}

func TestSettingsProbeInventoryQueryAndDetail(t *testing.T) {
	ctx, s := newStore(t)
	f := seedSettingsInventory(t, ctx, s)
	query := func(filter store.ProbeConfigFilter) ([]store.ProbeConfigInfo, int64) {
		t.Helper()
		rows, total, err := s.QueryProbeConfigs(ctx, filter)
		if err != nil {
			t.Fatalf("QueryProbeConfigs(%+v): %v", filter, err)
		}
		return rows, total
	}
	base := store.ProbeConfigFilter{Sort: "site", Order: "asc", Limit: 100}
	rows, total := query(base)
	if len(rows) != 4 || total != 4 {
		t.Fatalf("probe inventory = %d rows total %d", len(rows), total)
	}
	assignment := func(p store.ProbeConfigInfo) string {
		if p.Target != "" {
			return p.Target
		}
		return p.Mesh
	}
	for _, sortName := range []string{"site", "target", "type", "enabled", "updated"} {
		for _, order := range []string{"asc", "desc"} {
			filter := base
			filter.Sort, filter.Order = sortName, order
			got, gotTotal := query(filter)
			if gotTotal != 4 || !slices.IsSortedFunc(got, func(a, b store.ProbeConfigInfo) int {
				var primary int
				switch sortName {
				case "site":
					primary = strings.Compare(strings.ToLower(a.Site), strings.ToLower(b.Site))
				case "target":
					primary = strings.Compare(strings.ToLower(assignment(a)), strings.ToLower(assignment(b)))
				case "type":
					primary = strings.Compare(probeadmin.TypeName(a.ProbeType), probeadmin.TypeName(b.ProbeType))
				case "enabled":
					if a.Enabled != b.Enabled {
						if a.Enabled {
							primary = 1
						} else {
							primary = -1
						}
					}
				case "updated":
					primary = a.UpdatedAt.Compare(b.UpdatedAt)
				}
				return stableCompare(primary, a.ID, b.ID, order)
			}) {
				t.Errorf("probe sort %s %s unstable: %v", sortName, order, probeConfigIDs(got))
			}
		}
	}

	disabled := false
	filter := base
	filter.Query, filter.Mode, filter.Enabled, filter.ProbeType = "tcp", "direct", &disabled, 2
	rows, total = query(filter)
	if total != 1 || len(rows) != 1 || rows[0].ID != f.mgmtDirect {
		t.Errorf("combined probe filters = %v total %d", probeConfigIDs(rows), total)
	}
	filter.Query, filter.Mode, filter.Enabled, filter.ProbeType = "m-mgmt", "mesh", nil, 0
	if rows, total = query(filter); total != 1 || rows[0].ID != f.mgmtMesh {
		t.Errorf("mesh assignment search = %v total %d", probeConfigIDs(rows), total)
	}
	filter.Query = "tcp%_"
	if rows, total = query(filter); len(rows) != 0 || total != 0 {
		t.Errorf("literal probe wildcard = %v total %d", probeConfigIDs(rows), total)
	}
	filter = base
	filter.Networks = []uuid.UUID{f.mgmt}
	rows, total = query(filter)
	if total != 2 || slices.Contains(probeConfigIDs(rows), f.directID) || slices.Contains(probeConfigIDs(rows), f.tmplID) {
		t.Errorf("mgmt probes = %v total %d", probeConfigIDs(rows), total)
	}
	detail, err := s.GetProbeConfigScoped(ctx, f.mgmtDirect, []uuid.UUID{f.mgmt})
	if err != nil || detail.ID != f.mgmtDirect {
		t.Errorf("scoped probe detail = %+v err %v", detail, err)
	}
	if _, err := s.GetProbeConfigScoped(ctx, f.directID, []uuid.UUID{f.mgmt}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("inaccessible probe detail err = %v, want ErrNotFound", err)
	}
	probeadmin.TypeNames["future"] = 9
	defer delete(probeadmin.TypeNames, "future")
	if _, err := s.Pool().Exec(ctx, `UPDATE probe_configs SET probe_type = 9 WHERE id = $1`, f.mgmtDirect); err != nil {
		t.Fatalf("set future probe type: %v", err)
	}
	filter = base
	filter.Query, filter.ProbeType = "future", 9
	if rows, total = query(filter); total != 1 || len(rows) != 1 || rows[0].ID != f.mgmtDirect {
		t.Errorf("registry-derived future probe type = %v total %d", probeConfigIDs(rows), total)
	}

	filter = base
	filter.Sort, filter.Limit = "updated", 1
	var paged []uuid.UUID
	for offset := range 4 {
		filter.Offset = offset
		page, pageTotal := query(filter)
		if len(page) != 1 || pageTotal != 4 {
			t.Fatalf("probe page %d = %v total %d", offset, page, pageTotal)
		}
		paged = append(paged, page[0].ID)
	}
	if want := sortedUUIDs(paged...); !slices.Equal(paged, want) {
		t.Errorf("stable probe pages = %v, want %v", paged, want)
	}

	enabled := true
	for _, bad := range []store.ProbeConfigFilter{
		{Sort: "bogus", Order: "asc", Limit: 1},
		{Sort: "site", Order: "sideways", Limit: 1},
		{Sort: "site", Order: "asc", Mode: "bogus", Limit: 1},
		{Sort: "site", Order: "asc", ProbeType: -1, Limit: 1},
		{Sort: "site", Order: "asc", Enabled: &enabled, Limit: 0},
	} {
		if _, _, err := s.QueryProbeConfigs(ctx, bad); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("bad probe filter %+v: %v", bad, err)
		}
	}
}
