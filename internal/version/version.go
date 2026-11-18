// Package version reports which build of the runner is running.
package version

import "fmt"

// GoReleaser sets these through -ldflags -X, which can only write to package variables.
var (
	version = "dev"
	commit  = "unknown"
)

// Info identifies a build of the runner.
type Info struct {
	Version string
	Commit  string
}

// Get returns the version and commit of this build.
func Get() Info {
	return Info{Version: version, Commit: commit}
}

// String formats the build for people reading the output of the version command.
func (i Info) String() string {
	return fmt.Sprintf("backproof %s (commit %s)", i.Version, i.Commit)
}
