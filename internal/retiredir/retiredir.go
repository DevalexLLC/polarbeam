// Package retiredir moves a state directory aside under a timestamped
// name instead of deleting it. Both binaries use it for the operations
// that used to be an `rm -rf` or `mv` through `--entrypoint sh` on the
// old alpine images (server `ca retire`, agent `identity retire`): the
// shell-less release images cannot run those, and a rename keeps the
// retired directory as the rollback path.
package retiredir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Move renames dir to <dir>.retired-<UTC yyyymmddThhmmssZ> and returns the
// new path. dir is cleaned first: with a trailing slash the destination
// would otherwise be computed inside the directory being moved. It refuses
// a missing path, a non-directory, and an existing destination.
func Move(dir string, now time.Time) (string, error) {
	dir = filepath.Clean(dir)
	st, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("no directory at %s (nothing to retire)", dir)
		}
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	retired := dir + ".retired-" + now.UTC().Format("20060102T150405Z")
	if _, err := os.Stat(retired); err == nil {
		return "", fmt.Errorf("%s already exists", retired)
	}
	if err := os.Rename(dir, retired); err != nil {
		return "", err
	}
	return retired, nil
}
