package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

var testListSpec = listQuerySpec{
	Filters: []listFilterSpec{
		{Name: "kind", Allowed: []string{"agent", "external"}},
		{Name: "status", Allowed: []string{"healthy", "incident"}},
	},
	Sorts:        []string{"name", "created"},
	DefaultSort:  "name",
	DefaultOrder: "asc",
}

func TestReadListQueryModeAndValues(t *testing.T) {
	maxQuery := strings.Repeat("界", 200)
	tests := []struct {
		name       string
		raw        string
		wantMode   bool
		wantQuery  string
		wantFilter map[string]string
		wantSort   string
		wantOrder  string
		wantLimit  int
		wantOffset int
	}{
		{name: "legacy empty"},
		{name: "legacy endpoint parameter", raw: "window=7d"},
		{name: "explicit network", raw: "network=blue", wantMode: true, wantSort: "name", wantOrder: "asc", wantLimit: 100},
		{name: "empty network starts mode without narrowing", raw: "network=", wantMode: true, wantSort: "name", wantOrder: "asc", wantLimit: 100},
		{name: "empty shared parameter starts mode", raw: "q=", wantMode: true, wantSort: "name", wantOrder: "asc", wantLimit: 100},
		{name: "unicode query boundary", raw: "q=" + url.QueryEscape(maxQuery), wantMode: true, wantQuery: maxQuery, wantSort: "name", wantOrder: "asc", wantLimit: 100},
		{
			name: "all values", raw: "network=blue&q=%20edge%20&kind=external&status=incident&sort=created&order=desc&limit=1&offset=9",
			wantMode: true, wantQuery: "edge", wantFilter: map[string]string{"kind": "external", "status": "incident"},
			wantSort: "created", wantOrder: "desc", wantLimit: 1, wantOffset: 9,
		},
		{name: "upper limit boundary", raw: "limit=100&offset=0", wantMode: true, wantSort: "name", wantOrder: "asc", wantLimit: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/?"+tt.raw, nil)
			w := httptest.NewRecorder()
			got, ok := readListQuery(w, r, testListSpec)
			if !ok {
				t.Fatalf("readListQuery = %d: %s", w.Code, w.Body)
			}
			if got.Mode != tt.wantMode || got.Query != tt.wantQuery || got.Sort != tt.wantSort ||
				got.Order != tt.wantOrder || got.Limit != tt.wantLimit || got.Offset != tt.wantOffset {
				t.Errorf("query = %+v", got)
			}
			if len(got.Filters) != len(tt.wantFilter) {
				t.Fatalf("filters = %v, want %v", got.Filters, tt.wantFilter)
			}
			for name, value := range tt.wantFilter {
				if got.Filters[name] != value {
					t.Errorf("filter %s = %q, want %q", name, got.Filters[name], value)
				}
			}
		})
	}
}

func TestReadListQueryRejectsInvalidValues(t *testing.T) {
	tooLong := strings.Repeat("x", 201)
	tooManyRunes := strings.Repeat("界", 201)
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "ascii query", raw: "q=" + tooLong, want: "q must be at most 200 characters"},
		{name: "unicode query", raw: "q=" + url.QueryEscape(tooManyRunes), want: "q must be at most 200 characters"},
		{name: "empty filter", raw: "kind=", want: "kind must be one of: agent, external"},
		{name: "filter", raw: "status=down", want: "status must be one of: healthy, incident"},
		{name: "empty sort", raw: "sort=", want: "sort must be one of: name, created"},
		{name: "sort", raw: "sort=updated", want: "sort must be one of: name, created"},
		{name: "empty order", raw: "order=", want: "order must be one of: asc, desc"},
		{name: "order", raw: "order=sideways", want: "order must be one of: asc, desc"},
		{name: "zero limit", raw: "limit=0", want: "limit must be an integer between 1 and 100"},
		{name: "limit above maximum", raw: "limit=101", want: "limit must be an integer between 1 and 100"},
		{name: "empty limit", raw: "limit=", want: "limit must be an integer between 1 and 100"},
		{name: "noninteger limit", raw: "limit=one", want: "limit must be an integer between 1 and 100"},
		{name: "overflowing limit", raw: "limit=999999999999999999999999", want: "limit must be an integer between 1 and 100"},
		{name: "negative offset", raw: "offset=-1", want: "offset must be a non-negative integer"},
		{name: "empty offset", raw: "offset=", want: "offset must be a non-negative integer"},
		{name: "noninteger offset", raw: "offset=one", want: "offset must be a non-negative integer"},
		{name: "overflowing offset", raw: "offset=999999999999999999999999", want: "offset must be a non-negative integer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/?"+tt.raw, nil)
			w := httptest.NewRecorder()
			if _, ok := readListQuery(w, r, testListSpec); ok {
				t.Fatal("invalid query unexpectedly accepted")
			}
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusBadRequest || body["error"] != tt.want || len(body) != 1 {
				t.Errorf("response = %d %v, want 400 error %q", w.Code, body, tt.want)
			}
		})
	}
}

func TestReadListQueryUsesUniformErrorResponse(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/?kind=bad&sort=bad&order=bad&limit=0&offset=-1", nil)
	w := httptest.NewRecorder()
	if _, ok := readListQuery(w, r, testListSpec); ok {
		t.Fatal("invalid query unexpectedly accepted")
	}
	if w.Code != http.StatusBadRequest || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("response = %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := "kind must be one of: agent, external; sort must be one of: name, created; " +
		"order must be one of: asc, desc; limit must be an integer between 1 and 100; " +
		"offset must be a non-negative integer"
	if body["error"] != want || len(body) != 1 {
		t.Errorf("body = %v, want error %q", body, want)
	}
}

func TestListQueryPageMetadata(t *testing.T) {
	q := listQuery{Limit: 25}
	tests := []struct {
		name    string
		offset  int
		total   int64
		hasMore bool
	}{
		{name: "empty", total: 0},
		{name: "full first page", total: 25},
		{name: "partial first page", total: 9},
		{name: "middle page", offset: 25, total: 51, hasMore: true},
		{name: "final page", offset: 50, total: 51},
		{name: "past end", offset: 75, total: 51},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q.Offset = tt.offset
			got := q.page(tt.total)
			if got.Limit != 25 || got.Offset != tt.offset || got.Total != tt.total || got.HasMore != tt.hasMore {
				t.Errorf("page = %+v", got)
			}
		})
	}
	q.Offset = 75
	encoded, err := json.Marshal(q.page(101))
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"limit":25,"offset":75,"total":101,"has_more":true}` {
		t.Errorf("page JSON = %s", encoded)
	}
}

func TestListQueryScopeNarrowsAuthenticatedNetworks(t *testing.T) {
	f := newFakeDB()
	blue := store.NetworkRef{ID: uuid.New(), Name: "blue"}
	red := store.NetworkRef{ID: uuid.New(), Name: "red"}
	f.networks = append(f.networks,
		store.NetworkAdminInfo{ID: blue.ID, Name: blue.Name},
		store.NetworkAdminInfo{ID: red.ID, Name: red.Name})
	a := &api{db: db{f}}

	resolve := func(session *store.SessionInfo, network *string) ([]uuid.UUID, *httptest.ResponseRecorder, bool) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r = r.WithContext(withSessionCtx(context.Background(), session))
		q := listQuery{}
		if network != nil {
			q.Network, q.networkSet = *network, true
		}
		w := httptest.NewRecorder()
		ids, ok := a.listQueryScope(w, r, q)
		return ids, w, ok
	}

	global := &store.SessionInfo{Role: store.RoleAdmin}
	scoped := &store.SessionInfo{Role: store.RoleNetworkViewer, Networks: []store.NetworkRef{blue}}
	emptyScope := &store.SessionInfo{Role: store.RoleNetworkViewer}
	if ids, _, ok := resolve(global, nil); !ok || ids != nil {
		t.Errorf("global default scope = %v, %v; want nil, true", ids, ok)
	}
	if ids, _, ok := resolve(scoped, nil); !ok || !slices.Equal(ids, []uuid.UUID{blue.ID}) {
		t.Errorf("scoped default = %v, %v; want blue, true", ids, ok)
	}
	if ids, _, ok := resolve(emptyScope, nil); !ok || ids == nil || len(ids) != 0 {
		t.Errorf("empty scoped default = %v, %v; want non-nil empty, true", ids, ok)
	}
	if ids, _, ok := resolve(global, &blue.Name); !ok || !slices.Equal(ids, []uuid.UUID{blue.ID}) {
		t.Errorf("global blue = %v, %v; want blue, true", ids, ok)
	}
	if ids, _, ok := resolve(scoped, &blue.Name); !ok || !slices.Equal(ids, []uuid.UUID{blue.ID}) {
		t.Errorf("scoped blue = %v, %v; want blue, true", ids, ok)
	}

	_, foreign, foreignOK := resolve(scoped, &red.Name)
	unknownName := "missing"
	_, unknown, unknownOK := resolve(scoped, &unknownName)
	_, globalUnknown, globalUnknownOK := resolve(global, &unknownName)
	emptyNetwork := ""
	emptyGlobalIDs, emptyGlobal, emptyGlobalOK := resolve(global, &emptyNetwork)
	emptyScopedIDs, emptyScoped, emptyScopedOK := resolve(scoped, &emptyNetwork)
	if foreignOK || unknownOK || foreign.Code != http.StatusNotFound || unknown.Code != http.StatusNotFound {
		t.Fatalf("foreign/unknown = %d,%v / %d,%v; want 404,false", foreign.Code, foreignOK, unknown.Code, unknownOK)
	}
	if globalUnknownOK || globalUnknown.Code != http.StatusNotFound {
		t.Fatalf("global unknown = %d,%v; want 404,false", globalUnknown.Code, globalUnknownOK)
	}
	if !emptyGlobalOK || emptyGlobal.Code != http.StatusOK || emptyGlobalIDs != nil {
		t.Fatalf("empty global network = %v,%d,%v; want nil,200,true", emptyGlobalIDs, emptyGlobal.Code, emptyGlobalOK)
	}
	if !emptyScopedOK || emptyScoped.Code != http.StatusOK ||
		!slices.Equal(emptyScopedIDs, []uuid.UUID{blue.ID}) {
		t.Fatalf("empty scoped network = %v,%d,%v; want blue,200,true", emptyScopedIDs, emptyScoped.Code, emptyScopedOK)
	}
	for name, recorder := range map[string]*httptest.ResponseRecorder{"foreign": foreign, "unknown": unknown} {
		var body map[string]string
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s body: %v", name, err)
		}
		if !strings.HasSuffix(body["error"], "does not exist") {
			t.Errorf("%s error = %q, want unknown-network shape", name, body["error"])
		}
	}
}
