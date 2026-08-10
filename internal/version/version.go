// Package version carries build information injected via -ldflags.
package version

var (
	Version = "dev"
	Commit  = "none"
)

func String() string {
	return Version + " (" + Commit + ")"
}
