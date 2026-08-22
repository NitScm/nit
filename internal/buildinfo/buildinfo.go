// Package buildinfo carries the identity of a build.
//
// A released binary that cannot say which build it is turns every bug report
// into a guess. There are two ways one gets an identity, and both are covered
// here because both happen:
//
//   - A release sets these variables with -ldflags. That is what goreleaser
//     does, and it is the only way to record a tag.
//   - `go install github.com/NitScm/nit/cmd/nit@latest` sets no flags at all,
//     but the toolchain stamps the module version and, for a build from a
//     checkout, the VCS revision. Reading that back means an installed binary
//     still identifies itself.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Set by -ldflags at release time. Empty in every other build.
var (
	version = ""
	commit  = ""
	date    = ""
)

// Info is what a binary reports for -version.
type Info struct {
	Version  string
	Commit   string
	Date     string
	Go       string
	Platform string
}

// Get resolves the build identity, preferring what the release stamped and
// falling back to what the toolchain recorded.
func Get() Info {
	info := Info{
		Version:  version,
		Commit:   commit,
		Date:     date,
		Go:       runtime.Version(),
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}

	build, ok := debug.ReadBuildInfo()
	if !ok {
		return info.withDefaults()
	}

	// (devel) is what the toolchain records for a build that is not from a
	// tagged module version. It is less useful than the revision below, so it
	// is not treated as a version.
	if info.Version == "" && build.Main.Version != "" && build.Main.Version != "(devel)" {
		info.Version = build.Main.Version
	}

	for _, setting := range build.Settings {
		switch setting.Key {
		case "vcs.revision":
			if info.Commit == "" {
				info.Commit = setting.Value
			}
		case "vcs.time":
			if info.Date == "" {
				info.Date = setting.Value
			}
		case "vcs.modified":
			// A binary built from a dirty tree is not the commit it names, and
			// saying so costs one word and saves an afternoon.
			if setting.Value == "true" {
				info.Commit += "+dirty"
			}
		}
	}

	return info.withDefaults()
}

func (i Info) withDefaults() Info {
	if i.Version == "" {
		i.Version = "dev"
	}
	if i.Commit == "" {
		i.Commit = "unknown"
	}
	return i
}

// String is the single line a -version flag prints.
//
// The commit is shortened and dropped when the version already contains it:
// the toolchain synthesises pseudo-versions like v0.0.0-20260101120000-abc123
// for an untagged build, and printing the hash twice on one line is noise in
// exactly the output someone is copying into a bug report.
func (i Info) String() string {
	line := i.Version

	if short := shorten(i.Commit); short != "" && !strings.Contains(i.Version, strings.TrimSuffix(short, dirtySuffix)) {
		line += " (" + short + ")"
	}

	if i.Date != "" {
		line += " built " + i.Date
	}

	return line + fmt.Sprintf(" %s %s", i.Go, i.Platform)
}

const dirtySuffix = "+dirty"

// shorten trims a full object name to the twelve characters people actually
// paste, keeping the dirty marker if there is one.
func shorten(commit string) string {
	suffix := ""

	if strings.HasSuffix(commit, dirtySuffix) {
		commit, suffix = strings.TrimSuffix(commit, dirtySuffix), dirtySuffix
	}

	if len(commit) > 12 {
		commit = commit[:12]
	}

	return commit + suffix
}
