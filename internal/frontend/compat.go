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
// rx-python declares the same two constants and keeps its own window
// while it is paused.
// The viewer is a 0.x product, where a minor bump is allowed to break
// compatibility, so the window names minor lines one at a time rather
// than a whole major.
//
// Widen this when a viewer release is cut: the window names the
// releases this backend has been checked against, so a viewer published
// after it is refused and `rx serve` comes up with no interface at all.
// Both backends and the viewer's release checklist carry the same rule.
//
// The window starts at 0.8.0, the first viewer that speaks contract 2:
// contract 2.0 removed what earlier viewers read (the index analysis,
// GET /v1/detectors), and they know only contract major 1. It ends below
// 0.10.0: contract 2.1 only adds members (the journal object of an
// index, the reason a file is not text, the chain reason
// entries_overlap), so viewer 0.8.x keeps working and viewer 0.9.x is
// the one that shows them.
const (
	MinViewerVersion          = "0.8.0"
	MaxViewerVersionExclusive = "0.10.0"
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
	return semverInRange(v)
}

// semverInRange reports whether v lies inside
// MinViewerVersion <= v < MaxViewerVersionExclusive.
func semverInRange(v semver) bool {
	minV, _ := parseSemver(MinViewerVersion)
	maxV, _ := parseSemver(MaxViewerVersionExclusive)
	return compareSemver(v, minV) >= 0 && compareSemver(v, maxV) < 0
}

// supportedRange renders the range for messages: "0.8.0 <= v < 0.10.0".
func supportedRange() string {
	return MinViewerVersion + " <= v < " + MaxViewerVersionExclusive
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
