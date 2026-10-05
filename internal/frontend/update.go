package frontend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CheckInterval is how old a cached viewer's last check may grow before
// `rx serve` asks GitHub again for a newer release inside the range.
const CheckInterval = 24 * time.Hour

// releasesPerPage is the size of the one page of GitHub's release list
// a check reads. rx-viewer publishes a release every few weeks, so the
// newest release inside the range is always on the first page; reading
// one page bounds the check to one request.
const releasesPerPage = 100

// maxReleaseListBytes caps the release list body. One page of 100
// releases with their notes and assets is well under 1 MiB; the cap
// stops a broken or hostile API from making rx hold an unbounded body.
const maxReleaseListBytes = 8 << 20

// ServedReason says why the viewer `rx serve` serves is the one it is.
type ServedReason string

// The reasons Ensure and Update report.
const (
	// ServedFromCache: the cached viewer, either checked within the last
	// day or still the newest release inside the range.
	ServedFromCache ServedReason = "cache"
	// ServedUpdated: a newer release inside the range replaced the cached
	// viewer (or replaced one outside the range).
	ServedUpdated ServedReason = "updated"
	// ServedInstalled: there was no cached viewer; the newest release
	// inside the range was installed.
	ServedInstalled ServedReason = "installed"
	// ServedOverride: RX_FRONTEND_URL or RX_FRONTEND_VERSION chose it.
	ServedOverride ServedReason = "override"
	// ServedNone: no viewer is served; / redirects to /docs.
	ServedNone ServedReason = "none"
)

// Served describes the viewer `rx serve` serves once Ensure or Update
// returns.
type Served struct {
	// Version is the served viewer's version, "" when none is served or
	// the cache does not record one.
	Version string
	// Reason says why this viewer is served.
	Reason ServedReason
	// Replaced is the cached version an update replaced ("" otherwise).
	Replaced string
}

// servedDescriptions holds the human text for each reason; {version}
// and {replaced} are filled in by Describe.
var servedDescriptions = map[ServedReason]string{
	ServedFromCache: "viewer {version} (cached)",
	ServedUpdated:   "viewer {version} (updated from {replaced})",
	ServedInstalled: "viewer {version} (installed)",
	ServedOverride:  "viewer {version} (set by RX_FRONTEND_URL or RX_FRONTEND_VERSION)",
	ServedNone:      "no viewer (/ redirects to /docs)",
}

// Describe renders s for the `rx serve` start-up banner, for example
// "viewer 0.6.0 (updated from 0.2.0)".
func (s Served) Describe() string {
	text, ok := servedDescriptions[s.Reason]
	if !ok {
		text = "viewer {version} (" + string(s.Reason) + ")"
	}
	return strings.NewReplacer(
		"{version}", versionLabel(s.Version),
		"{replaced}", versionLabel(s.Replaced),
	).Replace(text)
}

// versionLabel names a version, or says it is not recorded.
func versionLabel(v string) string {
	if v == "" {
		return "of unknown version"
	}
	return v
}

// Ensure makes sure the viewer `rx serve` should serve is on disk and
// reports which one it is and why. With no override set, it asks GitHub
// for a newer release inside the range only when the last check is at
// least CheckInterval old (or unrecorded), or when the cached viewer lies
// outside the range.
//
// A non-nil error says what failed; the Served value is still accurate:
// a failed check keeps the cached viewer and reports ServedFromCache, so
// the caller reports the error once and starts the server either way.
func (m *Manager) Ensure(ctx context.Context) (Served, error) {
	return m.ensure(ctx, isCheckDue)
}

// Update is Ensure with the check run now, whatever the last check
// says: `rx serve --update-viewer`. An override still wins over it.
func (m *Manager) Update(ctx context.Context) (Served, error) {
	return m.ensure(ctx, func(*CacheMetadata, time.Time) bool { return true })
}

// Cached describes the viewer on disk without asking the network, for
// `rx serve --skip-frontend`.
func (m *Manager) Cached() Served {
	if !m.IsAvailable() {
		return Served{Reason: ServedNone}
	}
	return Served{Version: m.cachedVersion(), Reason: ServedFromCache}
}

// ensure is the common body of Ensure and Update. isDue decides whether a
// cached viewer inside the range is checked against GitHub now.
func (m *Manager) ensure(ctx context.Context, isDue func(*CacheMetadata, time.Time) bool) (Served, error) {
	if m.envURL != "" {
		return m.ensureFromURL(ctx)
	}
	if m.envVersion != "" {
		return m.ensurePinned(ctx)
	}
	return m.ensureFromReleases(ctx, isDue)
}

// ensureFromURL downloads RX_FRONTEND_URL, on every start.
func (m *Manager) ensureFromURL(ctx context.Context) (Served, error) {
	if err := m.Download(ctx, m.envURL, "custom"); err != nil {
		return m.Cached(), err
	}
	m.withheld.Store(false)
	return Served{Version: m.cachedVersion(), Reason: ServedOverride}, nil
}

// ensurePinned serves the RX_FRONTEND_VERSION release, downloading it
// when the cache holds another version. The range does not apply.
func (m *Manager) ensurePinned(ctx context.Context) (Served, error) {
	requested := strings.TrimPrefix(m.envVersion, "v")
	if m.bundleOnDisk() && strings.TrimPrefix(m.cachedVersion(), "v") == requested {
		return Served{Version: m.cachedVersion(), Reason: ServedOverride}, nil
	}
	if err := m.Download(ctx, m.directDownloadURL(m.envVersion), m.envVersion); err != nil {
		return m.Cached(), err
	}
	m.withheld.Store(false)
	return Served{Version: m.cachedVersion(), Reason: ServedOverride}, nil
}

// cachedBundle is what Ensure knows about the cache before it decides.
type cachedBundle struct {
	// onDisk: index.html and assets/ are present.
	onDisk bool
	// version from .metadata.json, "" when not recorded.
	version string
	// usable: on disk and inside the range. A version that is not
	// recorded or does not parse counts as inside, so a cache someone
	// filled by hand is still served.
	usable bool
}

// readCachedBundle inspects the cache.
func (m *Manager) readCachedBundle(md *CacheMetadata) cachedBundle {
	b := cachedBundle{onDisk: m.bundleOnDisk()}
	if md != nil {
		b.version = md.Version.Version
	}
	b.usable = b.onDisk && viewerVersionCompatible(b.version)
	return b
}

// ensureFromReleases serves the newest published release inside the
// range, checking GitHub when the cache is unusable or isDue says so.
//
// The decision, by the cache's state:
//
//	cache usable, check not due        → serve it, ask nothing
//	cache usable, check due            → list releases; install a newer
//	                                     one inside the range, else keep
//	                                     the cache; record last_check
//	cache missing or outside the range → list releases; install the
//	                                     newest one inside the range
//
// The check runs before the server binds, never beside it: Download
// replaces the served directory's files one by one, so a swap under a
// running server could hand a browser an index.html whose hashed assets
// are already gone. When a usable cache exists, the release listing is
// bounded by releaseCheckTimeout, so an offline host starts at most that
// much later.
func (m *Manager) ensureFromReleases(ctx context.Context, isDue func(*CacheMetadata, time.Time) bool) (Served, error) {
	// A metadata file that does not parse reads as none: the check runs
	// and the next install or recorded check rewrites it.
	md, _ := m.ReadMetadata()
	cached := m.readCachedBundle(md)
	now := time.Now()
	if cached.usable && !isDue(md, now) {
		return Served{Version: cached.version, Reason: ServedFromCache}, nil
	}

	listCtx, cancel := ctx, context.CancelFunc(func() {})
	if cached.usable {
		listCtx, cancel = context.WithTimeout(ctx, m.releaseCheckTimeout)
	}
	releases, err := m.fetchReleases(listCtx)
	cancel()
	if err != nil {
		return m.servedAfterFailedCheck(cached, md, now, fmt.Errorf("viewer update check: %w", err))
	}
	choice, err := newestInRange(releases)
	if err != nil {
		return m.servedAfterFailedCheck(cached, md, now, fmt.Errorf("viewer update check: %w", err))
	}
	if choice.newerOutsideRange != "" {
		m.Logger.Info("frontend_newer_release_outside_range",
			"tag", choice.newerOutsideRange,
			"supported", supportedRange(),
			"hint", "a newer rx accepts it; RX_FRONTEND_VERSION installs it anyway")
	}

	if cached.usable && !isNewerThanCached(choice.version, cached.version) {
		m.recordCheck(md, now)
		return Served{Version: cached.version, Reason: ServedFromCache}, nil
	}
	if err := m.Download(ctx, choice.distURL, choice.tag); err != nil {
		return m.servedAfterFailedCheck(cached, md, now, fmt.Errorf("install viewer %s: %w", choice.tag, err))
	}
	m.withheld.Store(false)
	if cached.onDisk {
		return Served{Version: m.cachedVersion(), Reason: ServedUpdated, Replaced: cached.version}, nil
	}
	return Served{Version: m.cachedVersion(), Reason: ServedInstalled}, nil
}

// servedAfterFailedCheck decides what is served when a check or an
// install failed. A usable cache stays and the check is recorded, so an
// offline host pays the listing timeout once a day, not on every start.
// A cache outside the range is withheld: serving it is the silent break
// the range exists to prevent.
func (m *Manager) servedAfterFailedCheck(cached cachedBundle, md *CacheMetadata, now time.Time, err error) (Served, error) {
	if cached.usable {
		m.recordCheck(md, now)
		return Served{Version: cached.version, Reason: ServedFromCache}, err
	}
	if cached.onDisk {
		m.withheld.Store(true)
	}
	return Served{Reason: ServedNone}, err
}

// recordCheck writes last_check into the cache metadata, keeping every
// other field. A write failure is logged: the next start checks again.
func (m *Manager) recordCheck(md *CacheMetadata, now time.Time) {
	var updated CacheMetadata
	if md != nil {
		updated = *md
	}
	updated.LastCheck = now.UTC().Format(time.RFC3339)
	if err := m.WriteMetadata(&updated); err != nil {
		m.Logger.Warn("frontend_last_check_not_saved", "error", err.Error())
	}
}

// cachedVersion reads the cached viewer's version from .metadata.json,
// "" when it is not recorded.
func (m *Manager) cachedVersion() string {
	md, err := m.ReadMetadata()
	if err != nil || md == nil {
		return ""
	}
	return md.Version.Version
}

// lastCheckLayouts are the forms last_check is read in: rx-go writes
// RFC 3339 in UTC; rx-python writes datetime.now().isoformat(), local
// time with no zone. Go's parser accepts a fractional second after the
// seconds field even when the layout has none.
var lastCheckLayouts = []struct {
	layout string
	zone   *time.Location
}{
	{time.RFC3339, time.UTC},
	{"2006-01-02T15:04:05", time.Local},
}

// isCheckDue reports whether a cached viewer whose metadata is md should
// be checked against GitHub at now: when last_check is missing,
// unreadable, in the future (a clock that moved back) or at least
// CheckInterval old.
func isCheckDue(md *CacheMetadata, now time.Time) bool {
	if md == nil {
		return true
	}
	for _, l := range lastCheckLayouts {
		at, err := time.ParseInLocation(l.layout, md.LastCheck, l.zone)
		if err != nil {
			continue
		}
		age := now.Sub(at)
		return age < 0 || age >= CheckInterval
	}
	return true
}

// isNewerThanCached reports whether release is newer than the cached
// version. A cached version that does not parse is replaced by any
// published release.
func isNewerThanCached(release semver, cachedVersion string) bool {
	cached, ok := parseSemver(cachedVersion)
	if !ok {
		return true
	}
	return compareSemver(release, cached) > 0
}

// rangeChoice is the release a check would install.
type rangeChoice struct {
	tag     string
	version semver
	distURL string
	// newerOutsideRange names the newest published release past the
	// range when it is newer than the chosen one, "" otherwise.
	newerOutsideRange string
}

// newestInRange picks the newest published release inside the range from
// GitHub's list. Drafts, releases marked as pre-releases and tags that
// are not a plain vX.Y.Z are skipped. The newest release inside the
// range must carry dist.tar.gz: falling back to an older one would hide
// a broken release for a day.
func newestInRange(releases []releaseInfo) (rangeChoice, error) {
	var best *releaseInfo
	var bestVersion, outsideVersion semver
	outsideTag := ""
	for i := range releases {
		r := &releases[i]
		v, ok := publishedVersion(r)
		if !ok {
			continue
		}
		if !semverInRange(v) {
			if outsideTag == "" || compareSemver(v, outsideVersion) > 0 {
				outsideTag, outsideVersion = r.TagName, v
			}
			continue
		}
		if best == nil || compareSemver(v, bestVersion) > 0 {
			best, bestVersion = r, v
		}
	}
	if best == nil {
		if outsideTag != "" {
			return rangeChoice{}, fmt.Errorf("no published viewer release inside %s (newest is %s; set RX_FRONTEND_VERSION to install it anyway)",
				supportedRange(), outsideTag)
		}
		return rangeChoice{}, fmt.Errorf("no published viewer release inside %s", supportedRange())
	}
	distURL := best.DistURL()
	if distURL == "" {
		return rangeChoice{}, fmt.Errorf("release %s has no dist.tar.gz asset", best.TagName)
	}
	choice := rangeChoice{tag: best.TagName, version: bestVersion, distURL: distURL}
	if outsideTag != "" && compareSemver(outsideVersion, bestVersion) > 0 {
		choice.newerOutsideRange = outsideTag
	}
	return choice, nil
}

// publishedVersion returns a release's version when it is published (not
// a draft, not a pre-release) and tagged as a plain vX.Y.Z.
func publishedVersion(r *releaseInfo) (semver, bool) {
	if r.Draft || r.Prerelease || strings.ContainsAny(r.TagName, "-+") {
		return semver{}, false
	}
	return parseSemver(r.TagName)
}

// fetchReleases reads the first page of the repository's release list,
// newest first. One request, a bounded body.
func (m *Manager) fetchReleases(ctx context.Context) ([]releaseInfo, error) {
	url := fmt.Sprintf("%s/repos/%s/releases?per_page=%d", m.APIBase, m.Repo, releasesPerPage)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := m.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, githubStatusError(resp)
	}

	var releases []releaseInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReleaseListBytes)).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decode release list: %w", err)
	}
	return releases, nil
}

// githubStatusError explains a non-200 answer; GitHub's rate limit (403
// or 429 with no requests left) is named as such.
func githubStatusError(resp *http.Response) error {
	isLimitStatus := resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests
	if !isLimitStatus || resp.Header.Get("X-RateLimit-Remaining") != "0" {
		return fmt.Errorf("github returned %d", resp.StatusCode)
	}
	resetUnix, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil {
		return fmt.Errorf("github rate limit reached (HTTP %d)", resp.StatusCode)
	}
	return fmt.Errorf("github rate limit reached (HTTP %d); it resets at %s",
		resp.StatusCode, time.Unix(resetUnix, 0).UTC().Format(time.RFC3339))
}
