// Package version gives kompensator builds a comparable identity so the fleet
// can answer "is this binary newer than that one?". A release build carries a
// semantic-version tag injected at build time; a dev build carries the commit
// time of the tree it was built from, read from the embedded VCS info. A
// release always sorts above any dev build, so a dev binary is never seen as an
// upgrade over a released one — a dev can never replace a prod version.
package version

import (
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

const devPrefix = "dev+"

// Info is a comparable build identity. The zero value means "unknown" and sorts
// below every real build; callers use IsZero to detect "no reference given".
type Info struct {
	release bool
	semver  [3]int    // major.minor.patch, when release
	built   time.Time // commit time, when dev
	rev     string    // short VCS revision, when dev
	raw     string    // original token, for display
}

// Current resolves the running binary's version. tag is the release version
// injected with -ldflags "-X main.buildVersion=<tag>"; when empty (a plain dev
// build) the version is derived from the embedded VCS build info.
func Current(tag string) Info {
	if v, ok := parseRelease(strings.TrimSpace(tag)); ok {
		return v
	}
	return buildInfoDev()
}

// Parse reconstructs an Info from the token another binary printed with
// `kompensator version`, so a controller can compare a node's version to its
// own. An unrecognized token becomes an unknown dev build (sorts oldest).
func Parse(token string) Info {
	token = strings.TrimSpace(token)
	if v, ok := parseRelease(token); ok {
		return v
	}
	if v, ok := parseDev(token); ok {
		return v
	}
	return Info{raw: token}
}

// Token renders the version as a single, space-free, round-trippable string
// suitable for `kompensator version` output and command-line passing.
func (v Info) Token() string {
	if v.release {
		return v.raw
	}
	if !v.built.IsZero() {
		return fmt.Sprintf("%s%s+%s", devPrefix, v.built.UTC().Format(time.RFC3339), v.rev)
	}
	if v.raw != "" {
		return v.raw
	}
	return "dev"
}

// IsZero reports whether the Info carries no version information at all (used to
// mean "no controller reference supplied").
func (v Info) IsZero() bool {
	return !v.release && v.built.IsZero() && v.raw == ""
}

// Compare orders two builds: +1 if a is newer than b, -1 if older, 0 if
// equivalent. A release is always newer than a dev build, so a dev binary never
// counts as an upgrade over a released one. Two releases compare by semver, two
// dev builds by commit time (an unknown time sorts oldest).
func Compare(a, b Info) int {
	if a.release != b.release {
		if a.release {
			return 1
		}
		return -1
	}
	if a.release {
		for i := 0; i < 3; i++ {
			switch {
			case a.semver[i] > b.semver[i]:
				return 1
			case a.semver[i] < b.semver[i]:
				return -1
			}
		}
		return 0
	}
	switch {
	case a.built.Equal(b.built):
		return 0
	case a.built.After(b.built):
		return 1
	default:
		return -1
	}
}

// parseRelease accepts a semantic-version tag, optionally with a git-describe
// suffix (v1.4.2-3-gabc123); ordering uses only the leading major.minor.patch.
func parseRelease(s string) (Info, bool) {
	if s == "" {
		return Info{}, false
	}
	core := s
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		core = core[:i]
	}
	core = strings.TrimPrefix(core, "v")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Info{}, false
	}
	var sv [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return Info{}, false
		}
		sv[i] = n
	}
	return Info{release: true, semver: sv, raw: s}, true
}

// parseDev accepts the dev token "dev+<RFC3339>+<rev>" produced by Token.
func parseDev(s string) (Info, bool) {
	if !strings.HasPrefix(s, devPrefix) {
		return Info{}, false
	}
	rest := strings.TrimPrefix(s, devPrefix)
	fields := strings.SplitN(rest, "+", 2)
	t, err := time.Parse(time.RFC3339, fields[0])
	if err != nil {
		return Info{}, false
	}
	rev := ""
	if len(fields) == 2 {
		rev = fields[1]
	}
	return Info{built: t, rev: rev, raw: s}, true
}

// buildInfoDev derives a dev version from the binary's embedded VCS info, so a
// plain `go build` yields a comparable, timestamped version without ldflags.
func buildInfoDev() Info {
	var v Info
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		v.raw = "dev"
		return v
	}
	var modified bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.time":
			if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
				v.built = t
			}
		case "vcs.revision":
			if len(s.Value) >= 7 {
				v.rev = s.Value[:7]
			} else {
				v.rev = s.Value
			}
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if modified {
		v.rev += "-dirty"
	}
	if v.rev == "" {
		v.rev = "unknown"
	}
	v.raw = v.Token()
	return v
}
