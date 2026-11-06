// Package buildinfo reports the version and source commit of the running
// binary.
package buildinfo

import "runtime/debug"

// Version is the Stavewell release. Release builds set it at build time:
//
//	go build -ldflags "-X github.com/glsmith3/stavewell/internal/buildinfo.Version=v0.1.0" ./cmd/stavewell
var Version = "dev"

// Commit returns the source commit the binary was built from. It appends
// "-modified" if the build included uncommitted changes, and returns
// "unknown" if no commit information was recorded (for example, with
// `go run` or in tests).
func Commit() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	return commitFromSettings(info.Settings)
}

func commitFromSettings(settings []debug.BuildSetting) string {
	var revision string
	modified := false
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision == "" {
		return "unknown"
	}
	if modified {
		return revision + "-modified"
	}
	return revision
}
