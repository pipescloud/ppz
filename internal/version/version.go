// Package version exposes the build-time version + sha to the CLI's
// `ppz version` verb. Both are settable via -ldflags at build time:
//
//	go build -ldflags "-X github.com/pipescloud/ppz/internal/version.Version=v0.3.0 \
//	                   -X github.com/pipescloud/ppz/internal/version.BuildSHA=abc1234" ./cmd/ppz
//
// Defaults are "dev" / "unknown" so a `go build ./...` without ldflags
// still produces a usable binary that self-identifies as a dev build.
//
// `go install github.com/pipescloud/ppz/cmd/ppz@vX.Y.Z` applies no
// ldflags, but the toolchain records the module version in the binary's
// build info. When Version was not stamped, init falls back to that so
// such a binary reports the release it was built from rather than "dev".
package version

import "runtime/debug"

var (
	Version  = "dev"
	BuildSHA = "unknown"
)

func init() {
	if Version != "dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		Version = fromModuleVersion(Version, info.Main.Version)
	}
}

// fromModuleVersion returns mod when it is a plain vX.Y.Z release tag,
// else current. Pseudo-versions, "+dirty" VCS stamps and "(devel)" are
// left alone so local and e2e builds keep reporting "dev".
func fromModuleVersion(current, mod string) string {
	if !isReleaseTag(mod) {
		return current
	}
	return mod
}

func isReleaseTag(v string) bool {
	if len(v) < 2 || v[0] != 'v' {
		return false
	}
	dots, digits := 0, 0
	for _, r := range v[1:] {
		switch {
		case r == '.':
			if digits == 0 {
				return false
			}
			dots, digits = dots+1, 0
		case r >= '0' && r <= '9':
			digits++
		default:
			return false
		}
	}
	return dots == 2 && digits > 0
}
