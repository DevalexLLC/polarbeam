package httpapi

// fakeDB network methods live here, next to the handlers they serve (the
// per-feature convention: siteconfig_test.go holds site+token fakes, etc.).

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

func (f *fakeDB) NetworkIDByName(_ context.Context, name string) (uuid.UUID, error) {
	for _, n := range f.networks {
		if n.Name == name {
			return n.ID, nil
		}
	}
	return uuid.Nil, fmt.Errorf("network %q does not exist%w", name, store.ErrNotFound)
}
