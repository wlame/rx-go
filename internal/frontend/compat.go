package frontend

import (
	"strconv"
	"strings"
)

// The range of rx-viewer releases this backend was built against.
//
// "latest" on GitHub can move to a viewer that expects response fields
// this backend does not send, and the bundle is served from the rx origin
// to the user's browser, so an automatic upgrade past the range is a
// silent break rather than a new feature. `RX_FRONTEND_VERSION` and
// `RX_FRONTEND_URL` override the check for an operator who knows better.
//
// rx-python declares the same two constants; they must be changed
// together.
// The viewer is a 0.x product, where a minor bump is allowed to break
// compatibility, so the window is one minor line wide rather than one
// major.
const (
	MinViewerVersion          = "0.2.0"
	MaxViewerVersionExclusive = "0.3.0"
)

// viewerVersionCompatible reports whether a viewer release may be
// installed automatically.
//
// A version that cannot be parsed is accepted: refusing on a parse
// failure would strand every backend the day a release is tagged in an
// unexpected shape, and the checksum is what actually protects the
// bundle's contents.
func viewerVersionCompatible(version string) bool {
	v, ok := parseSemver(version)
	if !ok {
		return true
	}
	minV, _ := parseSemver(MinViewerVersion)
	maxV, _ := parseSemver(MaxViewerVersionExclusive)
	return compareSemver(v, minV) >= 0 && compareSemver(v, maxV) < 0
}

// semver is the major.minor.patch triple; pre-release and build metadata
// are ignored, which is enough for release tags of the form vX.Y.Z.
type semver struct{ major, minor, patch int }

// parseSemver reads "1.2.3" or "v1.2.3". It returns ok=false for
// anything it cannot read as three numbers.
func parseSemver(s string) (semver, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" {
		return semver{}, false
	}
	// Drop any pre-release or build suffix.
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, false
		}
		out[i] = n
	}
	return semver{major: out[0], minor: out[1], patch: out[2]}, true
}

// compareSemver returns -1, 0 or 1.
func compareSemver(a, b semver) int {
	for _, pair := range [][2]int{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if pair[0] != pair[1] {
			if pair[0] < pair[1] {
				return -1
			}
			return 1
		}
	}
	return 0
}
