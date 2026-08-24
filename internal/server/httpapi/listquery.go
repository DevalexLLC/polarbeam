package httpapi

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	listDefaultLimit  = 100
	listMaxLimit      = 100
	listMaxQueryRunes = 200
)

type listFilterSpec struct {
	Name    string
	Allowed []string
}

// listQuerySpec is the endpoint-owned part of the shared list contract.
// Filters stay ordered so a request with multiple mistakes gets a stable,
// testable error response.
type listQuerySpec struct {
	Filters      []listFilterSpec
	Sorts        []string
	DefaultSort  string
	DefaultOrder string
}

// listQuery is the validated HTTP contract consumed by endpoint-specific
// store filters. Mode is false only when none of the shared or declared
// filter parameters was present, which preserves legacy full responses.
type listQuery struct {
	Mode       bool
	Network    string
	networkSet bool
	Query      string
	Filters    map[string]string
	Sort       string
	Order      string
	Limit      int
	Offset     int
}

type listPageJSON struct {
	Limit   int   `json:"limit"`
	Offset  int   `json:"offset"`
	Total   int64 `json:"total"`
	HasMore bool  `json:"has_more"`
}

func parseListQuery(values url.Values, spec listQuerySpec) (listQuery, error) {
	q := listQuery{
		Network:    values.Get("network"),
		networkSet: values.Get("network") != "",
		Query:      strings.TrimSpace(values.Get("q")),
		Filters:    make(map[string]string, len(spec.Filters)),
	}

	for _, name := range []string{"network", "q", "sort", "order", "limit", "offset"} {
		q.Mode = q.Mode || values.Has(name)
	}
	for _, filter := range spec.Filters {
		q.Mode = q.Mode || values.Has(filter.Name)
	}
	if q.Mode {
		q.Sort = spec.DefaultSort
		q.Order = spec.DefaultOrder
		q.Limit = listDefaultLimit
	}

	var problems []string
	if utf8.RuneCountInString(q.Query) > listMaxQueryRunes {
		problems = append(problems, fmt.Sprintf("q must be at most %d characters", listMaxQueryRunes))
	}
	for _, filter := range spec.Filters {
		if !values.Has(filter.Name) {
			continue
		}
		value := values.Get(filter.Name)
		if !slices.Contains(filter.Allowed, value) {
			problems = append(problems,
				fmt.Sprintf("%s must be one of: %s", filter.Name, strings.Join(filter.Allowed, ", ")))
			continue
		}
		q.Filters[filter.Name] = value
	}
	if values.Has("sort") {
		value := values.Get("sort")
		if !slices.Contains(spec.Sorts, value) {
			problems = append(problems, fmt.Sprintf("sort must be one of: %s", strings.Join(spec.Sorts, ", ")))
		} else {
			q.Sort = value
		}
	}
	if values.Has("order") {
		value := values.Get("order")
		if value != "asc" && value != "desc" {
			problems = append(problems, "order must be one of: asc, desc")
		} else {
			q.Order = value
		}
	}
	if values.Has("limit") {
		value, err := strconv.Atoi(values.Get("limit"))
		if err != nil || value < 1 || value > listMaxLimit {
			problems = append(problems,
				fmt.Sprintf("limit must be an integer between 1 and %d", listMaxLimit))
		} else {
			q.Limit = value
		}
	}
	if values.Has("offset") {
		value, err := strconv.Atoi(values.Get("offset"))
		if err != nil || value < 0 {
			problems = append(problems, "offset must be a non-negative integer")
		} else {
			q.Offset = value
		}
	}
	if len(problems) > 0 {
		return listQuery{}, fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return q, nil
}

// readListQuery writes the API's uniform 400 shape when parsing fails.
func readListQuery(w http.ResponseWriter, r *http.Request, spec listQuerySpec) (listQuery, bool) {
	q, err := parseListQuery(r.URL.Query(), spec)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return listQuery{}, false
	}
	return q, true
}

// listQueryScope narrows the caller's authenticated scope to an explicitly
// requested plane. The existing scope-first resolver makes an inaccessible
// plane indistinguishable from one that does not exist.
func (a *api) listQueryScope(w http.ResponseWriter, r *http.Request, q listQuery) ([]uuid.UUID, bool) {
	if !q.networkSet || q.Network == "" {
		return scopeIDs(r.Context()), true
	}
	id, ok := a.requireNetworkScopeName(w, r, q.Network)
	if !ok {
		return nil, false
	}
	return []uuid.UUID{id}, true
}

func (q listQuery) page(total int64) listPageJSON {
	remaining := total - int64(q.Offset)
	return listPageJSON{
		Limit: q.Limit, Offset: q.Offset, Total: total,
		HasMore: remaining > int64(q.Limit),
	}
}
