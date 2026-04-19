// Package hooks implements the webhook dispatch subsystem — the
// "async webhook notifications for trace events" feature from
// rx-python/src/rx/hooks.py.
//
// Three events are dispatched:
//
//   - on_file     — fired once per file after the scan completes
//   - on_match    — fired once per match (very hot; typically gated
//     by max_results)
//   - on_complete — fired once per full trace request
//
// Per user decision 6.9.2 (2026-04-18), the Go port is
// FIRE-AND-FORGET. No retry, no backoff, no acknowledgement: a single
// HTTP POST per event with a 3 s timeout. If the POST fails, log a
// warning and move on. Implementation uses a buffered channel +
// goroutine pool so the trace engine never blocks on a hook call.
//
// Security:
//
//   - Each hook URL is validated at configuration time (http/https
//     only, must parse as *url.URL).
//   - When RX_DISABLE_CUSTOM_HOOKS is set, request-level URL
//     overrides are rejected and only env-var hooks fire.
//   - SSRF guard covers loopback / link-local / RFC 1918 / CGNAT
//     (see internalHostReason).
//   - Option X (R2M5): DNS names are resolved at validation time and
//     the resulting IPs are checked against the internal-address
//     list.
//   - Option Z (R2M5): when RX_HOOK_STRICT_IP_ONLY=true, hostname
//     URLs are rejected wholesale — operators must supply IP
//     literals. This is the strongest defense against DNS
//     rebinding attacks (where DNS flips to an internal address
//     AFTER validation but BEFORE the POST).
package hooks

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

// ============================================================================
// Configuration
// ============================================================================

// HookConfig holds the three webhook URLs that are currently in effect
// for a single trace request. A zero-value (all-empty) HookConfig
// means "no webhooks configured" — dispatch calls become no-ops.
type HookConfig struct {
	OnFileURL     string
	OnMatchURL    string
	OnCompleteURL string
}

// HasAny reports whether at least one hook URL is set.
func (c HookConfig) HasAny() bool {
	return c.OnFileURL != "" || c.OnMatchURL != "" || c.OnCompleteURL != ""
}

// HasMatchHook reports whether on_match is configured. Useful for the
// engine hot loop: when on_match isn't wired, skip the per-match Go
// channel send entirely.
func (c HookConfig) HasMatchHook() bool { return c.OnMatchURL != "" }

// HookOverrides is the per-request override shape (subset of HookConfig).
// Passed in from the HTTP layer when a client explicitly sets hook
// parameters on a trace request. Each field is a pointer because nil
// means "don't override; use env default" and "" means "disable this
// hook for this request".
type HookOverrides struct {
	OnFileURL     *string
	OnMatchURL    *string
	OnCompleteURL *string
}

// ============================================================================
// Environment-driven defaults
// ============================================================================

// HookEnv captures the process-wide hook configuration. Created once
// at startup (or per-test via HookEnvFromEnv) and reused across every
// trace request.
type HookEnv struct {
	OnFileURL     string
	OnMatchURL    string
	OnCompleteURL string
	// DisableCustom mirrors RX_DISABLE_CUSTOM_HOOKS — when true,
	// per-request overrides are ignored and only env values fire.
	DisableCustom bool
}

// HookEnvFromEnv reads the current process environment and returns a
// HookEnv. Call once during server startup. Tests can build a HookEnv
// literal directly.
//
// Env vars (Python-compatible names):
//
//   - RX_HOOK_ON_FILE_URL
//   - RX_HOOK_ON_MATCH_URL
//   - RX_HOOK_ON_COMPLETE_URL
//   - RX_DISABLE_CUSTOM_HOOKS (truthy = disable per-request overrides)
func HookEnvFromEnv() HookEnv {
	return HookEnv{
		OnFileURL:     os.Getenv("RX_HOOK_ON_FILE_URL"),
		OnMatchURL:    os.Getenv("RX_HOOK_ON_MATCH_URL"),
		OnCompleteURL: os.Getenv("RX_HOOK_ON_COMPLETE_URL"),
		DisableCustom: parseBoolEnv(os.Getenv("RX_DISABLE_CUSTOM_HOOKS")),
	}
}

// parseBoolEnv recognizes the set of truthy strings Python does:
// "true", "yes", "1" (case-insensitive). Anything else is false.
func parseBoolEnv(v string) bool {
	switch strings.ToLower(v) {
	case "true", "yes", "1", "on":
		return true
	}
	return false
}

// ============================================================================
// Precedence
// ============================================================================

// EffectiveHooks returns the HookConfig actually used for a single
// request, given the process-wide env config and the per-request
// overrides (which may be nil).
//
// Precedence (matches rx-python/src/rx/hooks.py::get_effective_hooks):
//
//  1. If RX_DISABLE_CUSTOM_HOOKS is set → env values only; overrides
//     are silently dropped.
//  2. Otherwise, per-override: non-nil pointer value wins over env
//     default. Empty-string pointer value disables that hook for the
//     request.
func EffectiveHooks(env HookEnv, override HookOverrides) HookConfig {
	if env.DisableCustom {
		return HookConfig{
			OnFileURL:     env.OnFileURL,
			OnMatchURL:    env.OnMatchURL,
			OnCompleteURL: env.OnCompleteURL,
		}
	}
	return HookConfig{
		OnFileURL:     chooseString(override.OnFileURL, env.OnFileURL),
		OnMatchURL:    chooseString(override.OnMatchURL, env.OnMatchURL),
		OnCompleteURL: chooseString(override.OnCompleteURL, env.OnCompleteURL),
	}
}

// chooseString picks the override if non-nil, else the default. An
// empty-string override DOES win (that's how callers say "disable").
func chooseString(override *string, def string) string {
	if override == nil {
		return def
	}
	return *override
}

// ============================================================================
// Validation
// ============================================================================

// ErrInvalidHookURL is returned when a hook URL fails validation.
// Wrapped errors preserve the offending URL in the message.
var ErrInvalidHookURL = errors.New("invalid hook URL")

// cgnat100_64_10 is RFC 6598 CGNAT address space (100.64.0.0/10),
// which Go's net.IP.IsPrivate() does NOT recognize. Parsed once at
// package init; referenced from internalHostReason.
//
// R2M4 (2026-04-18): extending the SSRF guard to cover CGNAT. A
// webhook server sitting behind a carrier-grade NAT (common in
// some cloud deployments) exposes services in this range; treating
// it as public-facing was a latent SSRF hole.
var cgnat100_64_10 = mustParseCIDR("100.64.0.0/10")

func mustParseCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		// Package-init panic is fine for compile-time constants.
		panic(fmt.Sprintf("hooks: invalid CGNAT CIDR %q: %v", s, err))
	}
	return n
}

// dnsResolveTimeout caps how long ValidateURL waits on DNS before
// giving up and soft-accepting the hostname. 2 s matches typical
// DNS SLA; longer would let a slow resolver stall startup.
const dnsResolveTimeout = 2 * time.Second

// resolveHostFunc is the DNS resolver used by ValidateURL. Exposed
// as a package variable so tests can inject a deterministic
// resolver without touching the real network. Production callers
// use the default implementation that delegates to net.Resolver.
var resolveHostFunc = defaultResolveHost

// defaultResolveHost resolves a hostname to a list of IPs using the
// stdlib resolver with a short timeout. Returns the first error
// encountered; if the host doesn't resolve, the returned slice is
// empty and err is non-nil.
func defaultResolveHost(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.IP)
	}
	return out, nil
}

// ValidateURL rejects anything that isn't http or https. Empty input
// is OK (represents "hook not configured"); returns nil.
//
// Parity: Python uses HttpUrl from pydantic. httpx accepts http/https;
// we match that.
//
// SSRF PROTECTION LAYERS (Stage 8 Finding 15 + R2M4 + R2M5):
//
// URLs pointing at loopback addresses (127.x / ::1 / "localhost"),
// link-local space (169.254.x including the AWS IMDS 169.254.169.254),
// RFC 1918 private ranges (10/8, 172.16/12, 192.168/16), or the
// RFC 6598 CGNAT range (100.64/10) are REJECTED by default.
//
// Option X (R2M5): hostnames are also resolved at validation time;
// if DNS returns ANY internal address, the URL is rejected. This
// catches static DNS poisoning (attacker controls DNS, points
// evil.example.com → 127.0.0.1).
//
// Option Z (R2M5): when RX_HOOK_STRICT_IP_ONLY=true, hostname URLs
// are rejected UNCONDITIONALLY. Operators must supply IP literals.
// This is the strongest defense against DNS rebinding (where DNS
// flips AFTER validation, between validation and POST-time). Opt-in
// because it forces operators to maintain IP allowlists.
//
// Operators who need to hit internal endpoints (e.g. a sidecar
// logging collector) can still opt in with RX_ALLOW_INTERNAL_HOOKS=true.
// That bypasses ALL of the above guards — use with care.
func ValidateURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: %s (%v)", ErrInvalidHookURL, raw, err)
	}
	switch u.Scheme {
	case "http", "https":
		// ok
	default:
		return fmt.Errorf("%w: %s (scheme %q not allowed)",
			ErrInvalidHookURL, raw, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: %s (no host)", ErrInvalidHookURL, raw)
	}
	// Short-circuit: operator opt-in bypasses ALL further guards.
	if parseBoolEnv(os.Getenv("RX_ALLOW_INTERNAL_HOOKS")) {
		return nil
	}
	host := u.Hostname()

	// Option Z: strict-IP-only mode. Reject any hostname that isn't a
	// literal IP. We run this BEFORE the static internalHostReason
	// check so the strict-IP policy applies uniformly regardless of
	// what the hostname might resolve to.
	if parseBoolEnv(os.Getenv("RX_HOOK_STRICT_IP_ONLY")) {
		if net.ParseIP(host) == nil {
			return fmt.Errorf("%w: %s (RX_HOOK_STRICT_IP_ONLY is set — hostname %q must be an IP literal)",
				ErrInvalidHookURL, raw, host)
		}
		// Fall through: IP literal — still needs to clear the
		// internal-host guard below.
	}

	// Static guard: covers IP literals and the 'localhost' string.
	if reason := internalHostReason(host); reason != "" {
		return fmt.Errorf("%w: %s (%s — set RX_ALLOW_INTERNAL_HOOKS=true to allow)",
			ErrInvalidHookURL, raw, reason)
	}

	// Option X: DNS-resolution-time check. Skip if the host is already
	// an IP literal (the static guard above handled it). Skip if strict-
	// IP mode is on (we already proved it's an IP). Otherwise resolve
	// and reject if any returned IP is internal.
	if net.ParseIP(host) == nil {
		ctx, cancel := context.WithTimeout(context.Background(), dnsResolveTimeout)
		defer cancel()
		ips, err := resolveHostFunc(ctx, host)
		if err != nil {
			// Soft-accept on resolution failure: better to let a
			// DNS blip through than false-positive-reject every
			// validation during a transient outage. The runtime
			// POST path will fail naturally if the host is truly
			// unreachable. Operators who want hard-fail semantics
			// can enable strict-IP mode.
			return nil
		}
		for _, ip := range ips {
			if reason := internalIPReason(ip); reason != "" {
				return fmt.Errorf("%w: %s (DNS resolved %q → %s, %s)",
					ErrInvalidHookURL, raw, host, ip.String(), reason)
			}
		}
	}
	return nil
}

// internalHostReason returns a non-empty description if the host
// resolves to a loopback, link-local, private-network, or CGNAT
// address, or is the literal string "localhost". Empty string means
// "public-looking host, allowed at this layer."
//
// Behavior:
//
//   - Bare IP literals are parsed and checked via net.IP helpers
//     plus the CGNAT-range check.
//   - The hostname "localhost" is treated as loopback regardless of
//     what /etc/hosts says, because an operator typing "localhost"
//     clearly intends the local machine.
//   - Other DNS names pass this layer. The caller may then run the
//     Option X DNS-resolution check (ValidateURL does this).
//
// This is a conservative static check; the deeper DNS resolution
// is layered on top in ValidateURL itself.
func internalHostReason(host string) string {
	if host == "" {
		return ""
	}
	// Hostname "localhost" is special — recognized by most stdlibs
	// including Go's net package. Reject without resolving.
	if strings.EqualFold(host, "localhost") {
		return "points at localhost"
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Not an IP literal. Accept at this layer — the caller runs
		// the Option X DNS-resolution check separately.
		return ""
	}
	return internalIPReason(ip)
}

// internalIPReason returns a description if the given IP falls in
// any of the address spaces considered internal for SSRF purposes,
// or empty if the IP is routable-public.
//
// Checked categories (in order of most-to-least common attack vector):
//
//   - loopback (127/8, ::1) — local services, rx-viewer dev server
//   - link-local (169.254/16 including IMDS, fe80::/10) — cloud
//     metadata endpoints with IAM creds
//   - RFC 1918 private (10/8, 172.16/12, 192.168/16, fc00::/7) —
//     typical internal network
//   - CGNAT (100.64/10) — carrier-grade NAT, some cloud deployments
//   - unspecified (0.0.0.0, ::) — locally-routed by default
//
// Extracted from internalHostReason so the Option X resolution
// path can call it directly on each resolved IP.
func internalIPReason(ip net.IP) string {
	if ip == nil {
		return ""
	}
	if ip.IsLoopback() {
		return "points at a loopback address"
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		// 169.254.0.0/16 and fe80::/10 — includes AWS IMDS.
		return "points at a link-local address"
	}
	if ip.IsPrivate() {
		// 10/8, 172.16/12, 192.168/16, fc00::/7 per RFC 1918 / RFC 4193.
		return "points at a private-network address"
	}
	// R2M4: RFC 6598 CGNAT (100.64.0.0/10). net.IP.IsPrivate() does
	// NOT cover this range; we check it explicitly via the CIDR
	// parsed at package init.
	if cgnat100_64_10.Contains(ip) {
		return "points at a CGNAT address"
	}
	if ip.IsUnspecified() {
		// 0.0.0.0 or :: — routing on the local machine typically sends
		// these to the host's default interface. Still locally-directed.
		return "points at an unspecified address"
	}
	return ""
}

// ValidateConfig runs ValidateURL over all three fields and returns
// the first error, or nil.
func ValidateConfig(c HookConfig) error {
	for _, u := range []string{c.OnFileURL, c.OnMatchURL, c.OnCompleteURL} {
		if err := ValidateURL(u); err != nil {
			return err
		}
	}
	return nil
}

// ============================================================================
// Tunables
// ============================================================================

// DefaultTimeout is the HTTP client timeout for every hook POST. Matches
// Python's HOOK_TIMEOUT_SECONDS. Not adjustable — making this an env var
// would let operators accidentally block the scan pipeline.
const DefaultTimeout = 3 * time.Second

// DefaultQueueDepth is the buffered-channel size for the dispatcher.
// 512 is generous: on a 1000-file / 100-match-per-file trace, we'd
// push 1000 + 100000 events; at 3 s per failed POST, a smaller buffer
// would quickly block. 512 trims memory while keeping the normal
// case (live hook target) non-blocking.
const DefaultQueueDepth = 512

// DefaultWorkers is the number of goroutines consuming from the queue.
// 8 × typical worker limit (which is NumCPU) keeps HTTP latency from
// being the scan bottleneck. Tests inject smaller numbers for
// determinism.
const DefaultWorkers = 8
