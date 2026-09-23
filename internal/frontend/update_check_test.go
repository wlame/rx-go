package frontend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRelease is one release the fake GitHub lists.
type fakeRelease struct {
	tag        string
	draft      bool
	prerelease bool
	// noDist leaves the dist.tar.gz asset out of the release.
	noDist bool
}

// fakeGitHub answers the releases listing the way api.github.com does and
// serves each release's bundle and .sha256 sidecar. Each bundle's
// version.json carries the release's version, as a real release does.
type fakeGitHub struct {
	srv *httptest.Server
	// listHits counts requests to the releases listing.
	listHits atomic.Int32
	// downloadHits counts bundle downloads (sidecars not included).
	downloadHits atomic.Int32
}

const fakeReleasesPath = "/repos/wlame/rx-viewer/releases"

// newFakeGitHub starts a fake GitHub that lists releases in the given
// order (GitHub lists the newest first).
func newFakeGitHub(t *testing.T, releases []fakeRelease) *fakeGitHub {
	t.Helper()
	gh := &fakeGitHub{}
	bundles := map[string][]byte{}
	for _, r := range releases {
		bundles[r.tag] = buildFakeTarballV(strings.TrimPrefix(r.tag, "v"))
	}
	gh.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == fakeReleasesPath {
			gh.listHits.Add(1)
			_ = json.NewEncoder(w).Encode(gh.listing(releases))
			return
		}
		// /download/{tag}/dist.tar.gz and its .sha256 sidecar.
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/download/"), "/")
		if len(parts) != 2 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		bundle, ok := bundles[parts[0]]
		switch {
		case !ok:
			w.WriteHeader(http.StatusNotFound)
		case parts[1] == "dist.tar.gz":
			gh.downloadHits.Add(1)
			_, _ = w.Write(bundle)
		case parts[1] == "dist.tar.gz.sha256":
			sum := sha256.Sum256(bundle)
			_, _ = w.Write([]byte(hex.EncodeToString(sum[:]) + "  dist.tar.gz\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(gh.srv.Close)
	return gh
}

// listing renders releases as GitHub's JSON.
func (gh *fakeGitHub) listing(releases []fakeRelease) []map[string]any {
	out := make([]map[string]any, 0, len(releases))
	for _, r := range releases {
		assets := []map[string]any{}
		if !r.noDist {
			assets = append(assets, map[string]any{
				"name":                 "dist.tar.gz",
				"browser_download_url": gh.srv.URL + "/download/" + r.tag + "/dist.tar.gz",
			})
		}
		out = append(out, map[string]any{
			"tag_name":   r.tag,
			"draft":      r.draft,
			"prerelease": r.prerelease,
			"assets":     assets,
		})
	}
	return out
}

// seedCache writes a cached viewer of the given version, last checked at
// lastCheck (the zero time leaves last_check empty).
func seedCache(t *testing.T, dir, version string, lastCheck time.Time) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>"+version+"</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	md := &CacheMetadata{Version: Version{Version: version}}
	if !lastCheck.IsZero() {
		md.LastCheck = lastCheck.UTC().Format(time.RFC3339)
	}
	m := NewManager(Config{CacheDir: dir, Logger: silentLog()})
	if err := m.WriteMetadata(md); err != nil {
		t.Fatal(err)
	}
}

// managerFor builds a manager over dir that asks apiBase for releases,
// with no override set, logging into logged.
func managerFor(dir, apiBase string, logged *strings.Builder) *Manager {
	m := NewManager(Config{
		CacheDir: dir,
		APIBase:  apiBase,
		Logger:   slog.New(slog.NewTextHandler(logged, nil)),
	})
	m.envURL = ""
	m.envVersion = ""
	return m
}

// servedIndex returns the cached index.html, which names its version.
func servedIndex(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	return string(b)
}

// requireRecentLastCheck fails unless the cache records a check within
// the last minute.
func requireRecentLastCheck(t *testing.T, m *Manager) {
	t.Helper()
	md, err := m.ReadMetadata()
	if err != nil || md == nil {
		t.Fatalf("ReadMetadata: md=%v err=%v", md, err)
	}
	at, err := time.Parse(time.RFC3339, md.LastCheck)
	if err != nil {
		t.Fatalf("last_check %q: %v", md.LastCheck, err)
	}
	if age := time.Since(at); age > time.Minute || age < -time.Minute {
		t.Errorf("last_check = %s, want about now", md.LastCheck)
	}
}

// Versions around the range's upper bound, derived from compat.go so
// these tests keep their meaning when the range is widened. The comments
// give the values for the range 0.2.0 <= v < 0.7.0.
var (
	upperBound, _     = parseSemver(MaxViewerVersionExclusive)
	firstPastRange    = fmt.Sprintf("%d.%d.0", upperBound.major, upperBound.minor)   // 0.7.0
	patchPastRange    = fmt.Sprintf("%d.%d.1", upperBound.major, upperBound.minor)   // 0.7.1
	pinnedPastRange   = fmt.Sprintf("%d.%d.5", upperBound.major, upperBound.minor)   // 0.7.5
	newestInsideRange = fmt.Sprintf("%d.%d.0", upperBound.major, upperBound.minor-1) // 0.6.0
	olderInsideRange  = fmt.Sprintf("%d.%d.0", upperBound.major, upperBound.minor-2) // 0.5.0
)

// windowReleases lists one release past the window first, as GitHub
// would once a newer viewer is out.
var windowReleases = []fakeRelease{
	{tag: "v" + firstPastRange}, {tag: "v" + newestInsideRange}, {tag: "v" + olderInsideRange},
}

func TestEnsure_StaleCheckInstallsNewestReleaseInsideWindow(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t, windowReleases)
	dir := t.TempDir()
	seedCache(t, dir, "0.2.0", time.Now().Add(-48*time.Hour))
	var logged strings.Builder
	m := managerFor(dir, gh.srv.URL, &logged)

	served, err := m.Ensure(context.Background())

	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	want := Served{Version: newestInsideRange, Reason: ServedUpdated, Replaced: "0.2.0"}
	if served != want {
		t.Errorf("served = %+v, want %+v", served, want)
	}
	if got := servedIndex(t, dir); got != "<html>"+newestInsideRange+"</html>" {
		t.Errorf("index.html = %q, want the %s bundle", got, newestInsideRange)
	}
	requireRecentLastCheck(t, m)
}

func TestEnsure_RecentCheckAsksNothing(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t, windowReleases)
	dir := t.TempDir()
	seedCache(t, dir, "0.2.0", time.Now().Add(-time.Hour))
	var logged strings.Builder
	m := managerFor(dir, gh.srv.URL, &logged)

	served, err := m.Ensure(context.Background())

	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if want := (Served{Version: "0.2.0", Reason: ServedFromCache}); served != want {
		t.Errorf("served = %+v, want %+v", served, want)
	}
	if hits := gh.listHits.Load(); hits != 0 {
		t.Errorf("releases listed %d times, want 0 within a day of the last check", hits)
	}
}

func TestUpdate_ChecksWhateverLastCheckSays(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t, windowReleases)
	dir := t.TempDir()
	seedCache(t, dir, "0.2.0", time.Now().Add(-time.Hour))
	var logged strings.Builder
	m := managerFor(dir, gh.srv.URL, &logged)

	served, err := m.Update(context.Background())

	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if hits := gh.listHits.Load(); hits != 1 {
		t.Errorf("releases listed %d times, want 1", hits)
	}
	if want := (Served{Version: newestInsideRange, Reason: ServedUpdated, Replaced: "0.2.0"}); served != want {
		t.Errorf("served = %+v, want %+v", served, want)
	}
}

func TestEnsure_FailedCheckKeepsTheCache(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// apiBase returns the API root to use; it may start servers.
		apiBase func(t *testing.T) string
		// wantInErr is a fragment the returned error must hold.
		wantInErr string
	}{
		{
			name: "offline",
			apiBase: func(t *testing.T) string {
				srv := httptest.NewServer(http.NotFoundHandler())
				srv.Close() // nothing listens at this address any more
				return srv.URL
			},
			wantInErr: "viewer update check",
		},
		{
			name:      "server error",
			apiBase:   statusAPI(http.StatusInternalServerError, nil),
			wantInErr: "500",
		},
		{
			name:      "rate limit",
			apiBase:   statusAPI(http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}),
			wantInErr: "rate limit",
		},
		{
			name: "newest release inside the window has no bundle",
			apiBase: func(t *testing.T) string {
				return newFakeGitHub(t, []fakeRelease{{tag: "v" + newestInsideRange, noDist: true}, {tag: "v" + olderInsideRange}}).srv.URL
			},
			wantInErr: "dist.tar.gz",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			seedCache(t, dir, "0.2.0", time.Now().Add(-48*time.Hour))
			var logged strings.Builder
			m := managerFor(dir, tc.apiBase(t), &logged)

			served, err := m.Ensure(context.Background())

			if err == nil || !strings.Contains(err.Error(), tc.wantInErr) {
				t.Fatalf("Ensure error = %v, want one naming %q", err, tc.wantInErr)
			}
			if want := (Served{Version: "0.2.0", Reason: ServedFromCache}); served != want {
				t.Errorf("served = %+v, want %+v", served, want)
			}
			if !m.IsAvailable() || servedIndex(t, dir) != "<html>0.2.0</html>" {
				t.Errorf("the cached viewer must stay in place and be served")
			}
			// The caller reports the returned error; the manager adds no
			// warning of its own, so the failure is logged once.
			if strings.Contains(logged.String(), "level=WARN") || strings.Contains(logged.String(), "level=ERROR") {
				t.Errorf("manager logged the failure itself:\n%s", logged.String())
			}
			requireRecentLastCheck(t, m)
		})
	}
}

// statusAPI returns an apiBase func for a server that answers every
// request with status and headers.
func statusAPI(status int, headers map[string]string) func(t *testing.T) string {
	return func(t *testing.T) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			for k, v := range headers {
				w.Header().Set(k, v)
			}
			w.WriteHeader(status)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
}

func TestEnsure_FailedCheckWaitsADayBeforeTheNextOne(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(api.Close)
	dir := t.TempDir()
	seedCache(t, dir, "0.2.0", time.Time{})
	var logged strings.Builder
	m := managerFor(dir, api.URL, &logged)

	if _, err := m.Ensure(context.Background()); err == nil {
		t.Fatal("first Ensure: want the failed check reported")
	}
	served, err := m.Ensure(context.Background())

	if err != nil {
		t.Errorf("second Ensure: %v, want no check within a day", err)
	}
	if hits.Load() != 1 {
		t.Errorf("API asked %d times, want 1", hits.Load())
	}
	if served.Reason != ServedFromCache {
		t.Errorf("served = %+v, want the cache", served)
	}
}

func TestEnsure_SlowReleaseListingIsCutShort(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); api.Close() })
	dir := t.TempDir()
	seedCache(t, dir, "0.2.0", time.Now().Add(-48*time.Hour))
	var logged strings.Builder
	m := managerFor(dir, api.URL, &logged)
	m.releaseCheckTimeout = 50 * time.Millisecond

	served, err := m.Ensure(context.Background())

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Ensure error = %v, want the listing deadline", err)
	}
	if served.Reason != ServedFromCache {
		t.Errorf("served = %+v, want the cache", served)
	}
}

func TestEnsure_NoCacheInstallsNewestReleaseInsideWindow(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t, windowReleases)
	dir := t.TempDir()
	var logged strings.Builder
	m := managerFor(dir, gh.srv.URL, &logged)

	served, err := m.Ensure(context.Background())

	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if want := (Served{Version: newestInsideRange, Reason: ServedInstalled}); served != want {
		t.Errorf("served = %+v, want %+v", served, want)
	}
	if got := servedIndex(t, dir); got != "<html>"+newestInsideRange+"</html>" {
		t.Errorf("index.html = %q, want the %s bundle", got, newestInsideRange)
	}
}

func TestEnsure_SkipsDraftsAndPreReleases(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t, []fakeRelease{
		{tag: "v0.6.2", draft: true},
		{tag: "v0.6.1", prerelease: true},
		{tag: "v0.6.0-rc.1"}, // a pre-release tag GitHub was not told about
		{tag: "not-a-version"},
		{tag: "v0.5.0"},
	})
	dir := t.TempDir()
	var logged strings.Builder
	m := managerFor(dir, gh.srv.URL, &logged)

	served, err := m.Ensure(context.Background())

	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if served.Version != "0.5.0" {
		t.Errorf("installed %q, want 0.5.0, the newest published release", served.Version)
	}
}

func TestEnsure_CurrentCacheIsKeptAndTheCheckRecorded(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t, windowReleases)
	dir := t.TempDir()
	seedCache(t, dir, newestInsideRange, time.Now().Add(-48*time.Hour))
	var logged strings.Builder
	m := managerFor(dir, gh.srv.URL, &logged)

	served, err := m.Ensure(context.Background())

	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if want := (Served{Version: newestInsideRange, Reason: ServedFromCache}); served != want {
		t.Errorf("served = %+v, want %+v", served, want)
	}
	if hits := gh.downloadHits.Load(); hits != 0 {
		t.Errorf("downloaded %d bundles, want 0 when the cache is the newest", hits)
	}
	requireRecentLastCheck(t, m)
}

func TestEnsure_CacheOutsideWindowIsReplaced(t *testing.T) {
	t.Parallel()
	for _, cached := range []string{"0.1.0", patchPastRange} {
		t.Run(cached, func(t *testing.T) {
			t.Parallel()
			gh := newFakeGitHub(t, windowReleases)
			dir := t.TempDir()
			// Checked an hour ago: a cache outside the window is replaced
			// whatever last_check says.
			seedCache(t, dir, cached, time.Now().Add(-time.Hour))
			var logged strings.Builder
			m := managerFor(dir, gh.srv.URL, &logged)

			served, err := m.Ensure(context.Background())

			if err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			if want := (Served{Version: newestInsideRange, Reason: ServedUpdated, Replaced: cached}); served != want {
				t.Errorf("served = %+v, want %+v", served, want)
			}
		})
	}
}

func TestEnsure_CacheOutsideWindowIsNotServedWhenItCannotBeReplaced(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedCache(t, dir, patchPastRange, time.Now().Add(-time.Hour))
	var logged strings.Builder
	m := managerFor(dir, statusAPI(http.StatusInternalServerError, nil)(t), &logged)

	served, err := m.Ensure(context.Background())

	if err == nil {
		t.Error("Ensure: want the failed check reported")
	}
	if want := (Served{Reason: ServedNone}); served != want {
		t.Errorf("served = %+v, want %+v", served, want)
	}
	if m.IsAvailable() {
		t.Error("a cached viewer outside the window must not be served")
	}
}

func TestEnsure_OverridesNeverListReleases(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t, windowReleases)
	cases := []struct {
		name   string
		cached string
		setEnv func(m *Manager)
		want   Served
	}{
		{
			name:   "RX_FRONTEND_VERSION pins the cached viewer, even outside the window",
			cached: pinnedPastRange,
			setEnv: func(m *Manager) { m.envVersion = "v" + pinnedPastRange },
			want:   Served{Version: pinnedPastRange, Reason: ServedOverride},
		},
		{
			name:   "RX_FRONTEND_URL downloads its bundle",
			cached: "0.2.0",
			setEnv: func(m *Manager) { m.envURL = gh.srv.URL + "/download/v" + newestInsideRange + "/dist.tar.gz" },
			want:   Served{Version: newestInsideRange, Reason: ServedOverride},
		},
	}
	for _, tc := range cases {
		for _, call := range []struct {
			name string
			run  func(*Manager, context.Context) (Served, error)
		}{{"Ensure", (*Manager).Ensure}, {"Update", (*Manager).Update}} {
			t.Run(tc.name+"/"+call.name, func(t *testing.T) {
				dir := t.TempDir()
				seedCache(t, dir, tc.cached, time.Now().Add(-48*time.Hour))
				var logged strings.Builder
				m := managerFor(dir, gh.srv.URL, &logged)
				tc.setEnv(m)
				before := gh.listHits.Load()

				served, err := call.run(m, context.Background())

				if err != nil {
					t.Fatalf("%s: %v", call.name, err)
				}
				if served != tc.want {
					t.Errorf("served = %+v, want %+v", served, tc.want)
				}
				if gh.listHits.Load() != before {
					t.Error("an override must not list releases")
				}
				if !m.IsAvailable() {
					t.Error("the override's viewer must be served")
				}
			})
		}
	}
}

func TestEnsure_ReleaseListingBodyIsBounded(t *testing.T) {
	t.Parallel()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A valid JSON array padded past the limit with whitespace.
		_, _ = w.Write([]byte("["))
		_, _ = w.Write([]byte(strings.Repeat(" ", maxReleaseListBytes)))
		_, _ = w.Write([]byte("]"))
	}))
	t.Cleanup(api.Close)
	var logged strings.Builder
	m := managerFor(t.TempDir(), api.URL, &logged)

	_, err := m.fetchReleases(context.Background())

	if err == nil {
		t.Error("fetchReleases: want an error for a listing past the size limit")
	}
}

func TestCheckDue(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		md        *CacheMetadata
		wantIsDue bool
	}{
		{"no metadata", nil, true},
		{"no last_check", &CacheMetadata{}, true},
		{"unreadable last_check", &CacheMetadata{LastCheck: "yesterday"}, true},
		{"two days old", &CacheMetadata{LastCheck: now.Add(-48 * time.Hour).Format(time.RFC3339)}, true},
		{"exactly a day old", &CacheMetadata{LastCheck: now.Add(-24 * time.Hour).Format(time.RFC3339)}, true},
		{"an hour old", &CacheMetadata{LastCheck: now.Add(-time.Hour).Format(time.RFC3339)}, false},
		{
			"an hour old, written by rx-python without a zone",
			&CacheMetadata{LastCheck: now.Add(-time.Hour).In(time.Local).Format("2006-01-02T15:04:05.000000")},
			false,
		},
		{"in the future", &CacheMetadata{LastCheck: now.Add(2 * time.Hour).Format(time.RFC3339)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isCheckDue(tc.md, now); got != tc.wantIsDue {
				t.Errorf("isCheckDue = %v, want %v", got, tc.wantIsDue)
			}
		})
	}
}

func TestServed_Describe(t *testing.T) {
	t.Parallel()
	cases := []struct {
		served Served
		want   string
	}{
		{Served{Version: "0.2.0", Reason: ServedFromCache}, "viewer 0.2.0 (cached)"},
		{Served{Version: "0.6.0", Reason: ServedUpdated, Replaced: "0.2.0"}, "viewer 0.6.0 (updated from 0.2.0)"},
		{Served{Version: "0.6.0", Reason: ServedInstalled}, "viewer 0.6.0 (installed)"},
		{Served{Version: "0.7.5", Reason: ServedOverride}, "viewer 0.7.5 (set by RX_FRONTEND_URL or RX_FRONTEND_VERSION)"},
		{Served{Reason: ServedNone}, "no viewer (/ redirects to /docs)"},
		{Served{Reason: ServedFromCache}, "viewer of unknown version (cached)"},
	}
	for _, tc := range cases {
		if got := tc.served.Describe(); got != tc.want {
			t.Errorf("Describe(%+v) = %q, want %q", tc.served, got, tc.want)
		}
	}
}
