package hooks

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestEffectiveHooks_Precedence(t *testing.T) {
	env := HookEnv{
		OnFileURL:     "http://env-file.local",
		OnMatchURL:    "http://env-match.local",
		OnCompleteURL: "http://env-complete.local",
	}
	// Case: no override — pure env values pass through.
	got := EffectiveHooks(env, HookOverrides{})
	if got.OnFileURL != env.OnFileURL {
		t.Errorf("no-override OnFile = %q, want %q", got.OnFileURL, env.OnFileURL)
	}
	// Case: override wins.
	custom := "http://custom.local"
	got = EffectiveHooks(env, HookOverrides{OnMatchURL: &custom})
	if got.OnMatchURL != custom {
		t.Errorf("override OnMatch = %q, want %q", got.OnMatchURL, custom)
	}
	// Env default still applies to OnFile.
	if got.OnFileURL != env.OnFileURL {
		t.Errorf("OnFile should inherit env, got %q", got.OnFileURL)
	}
	// Case: empty-string override means "disable for this request".
	empty := ""
	got = EffectiveHooks(env, HookOverrides{OnFileURL: &empty})
	if got.OnFileURL != "" {
		t.Errorf("empty override should disable; got %q", got.OnFileURL)
	}
}

func TestEffectiveHooks_DisableCustomDropsOverrides(t *testing.T) {
	env := HookEnv{
		OnFileURL:     "http://env-file.local",
		OnMatchURL:    "http://env-match.local",
		DisableCustom: true,
	}
	custom := "http://attacker.local"
	got := EffectiveHooks(env, HookOverrides{
		OnFileURL:  &custom,
		OnMatchURL: &custom,
	})
	if got.OnFileURL != env.OnFileURL {
		t.Errorf("OnFile leaked override: %q", got.OnFileURL)
	}
	if got.OnMatchURL != env.OnMatchURL {
		t.Errorf("OnMatch leaked override: %q", got.OnMatchURL)
	}
}

func TestHookConfig_HasAny_HasMatchHook(t *testing.T) {
	empty := HookConfig{}
	if empty.HasAny() {
		t.Error("empty.HasAny() should be false")
	}
	if empty.HasMatchHook() {
		t.Error("empty.HasMatchHook() should be false")
	}
	c := HookConfig{OnFileURL: "http://x"}
	if !c.HasAny() {
		t.Error("c.HasAny() should be true when file URL set")
	}
	if c.HasMatchHook() {
		t.Error("HasMatchHook should be false when match URL unset")
	}
	c.OnMatchURL = "http://y"
	if !c.HasMatchHook() {
		t.Error("HasMatchHook should be true when match URL set")
	}
}

func TestValidateURL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"empty_ok", "", false},
		{"http", "http://example.com/hook", false},
		{"https", "https://example.com/hook", false},
		{"https_with_port_and_path", "https://example.com:8080/a/b?c=d", false},
		{"missing_host", "http:///path", true},
		{"bad_scheme", "ftp://example.com", true},
		{"garbage", "::::", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateURL(tc.in)
			gotErr := err != nil
			if gotErr != tc.wantErr {
				t.Errorf("ValidateURL(%q): err=%v, wantErr=%v", tc.in, err, tc.wantErr)
			}
			if gotErr && !errors.Is(err, ErrInvalidHookURL) {
				t.Errorf("err = %v, want wrapped ErrInvalidHookURL", err)
			}
		})
	}
}

// TestValidateURL_RejectsInternalTargetsWithoutOptIn covers Stage 8
// Reviewer 2 High #15 / Finding 15: webhook URLs pointing at
// loopback, link-local, or RFC1918 private address space are a
// classic SSRF vector. A malicious user could configure a hook URL
// like http://169.254.169.254/latest/meta-data/iam/security-credentials/
// and receive AWS IMDS secrets over the wire as the hook fires.
//
// Expected policy: reject these destinations by default. Operators
// who genuinely need hooks that hit localhost (e.g. a sidecar
// logging service) can opt in by setting RX_ALLOW_INTERNAL_HOOKS=true.
//
// This test sits alongside the hostname/scheme checks; each case
// MUST fail without the env var, and succeed with it set.
func TestValidateURL_RejectsInternalTargetsWithoutOptIn(t *testing.T) {
	// Clean slate — reset any test-environment pollution.
	t.Setenv("RX_ALLOW_INTERNAL_HOOKS", "")

	internal := []struct {
		name string
		url  string
	}{
		{"loopback_v4", "http://127.0.0.1/hook"},
		{"loopback_hostname", "http://localhost/hook"},
		{"loopback_v6", "http://[::1]/hook"},
		{"link_local_imds", "http://169.254.169.254/latest/meta-data/"},
		{"rfc1918_10", "http://10.0.0.1/hook"},
		{"rfc1918_172", "http://172.16.0.5/hook"},
		{"rfc1918_192", "http://192.168.1.1/hook"},
	}
	for _, tc := range internal {
		tc := tc
		t.Run("reject_"+tc.name, func(t *testing.T) {
			err := ValidateURL(tc.url)
			if err == nil {
				t.Errorf("ValidateURL(%q) accepted an internal URL — SSRF risk. Want rejection.", tc.url)
			}
			if err != nil && !errors.Is(err, ErrInvalidHookURL) {
				t.Errorf("ValidateURL(%q) err=%v, want ErrInvalidHookURL wrapper", tc.url, err)
			}
		})
	}

	// Same URLs must be ACCEPTED when the opt-in env var is set.
	t.Setenv("RX_ALLOW_INTERNAL_HOOKS", "true")
	for _, tc := range internal {
		tc := tc
		t.Run("accept_with_optin_"+tc.name, func(t *testing.T) {
			if err := ValidateURL(tc.url); err != nil {
				t.Errorf("ValidateURL(%q) with RX_ALLOW_INTERNAL_HOOKS=true still rejected: %v",
					tc.url, err)
			}
		})
	}
}

func TestHookEnvFromEnv(t *testing.T) {
	t.Setenv("RX_HOOK_ON_FILE_URL", "http://file")
	t.Setenv("RX_HOOK_ON_MATCH_URL", "http://match")
	t.Setenv("RX_HOOK_ON_COMPLETE_URL", "http://complete")
	t.Setenv("RX_DISABLE_CUSTOM_HOOKS", "1")
	env := HookEnvFromEnv()
	if env.OnFileURL != "http://file" {
		t.Errorf("OnFileURL = %q", env.OnFileURL)
	}
	if !env.DisableCustom {
		t.Errorf("DisableCustom = false, want true")
	}
}

func TestParseBoolEnv(t *testing.T) {
	for _, v := range []string{"true", "YES", "1", "on", "True"} {
		if !parseBoolEnv(v) {
			t.Errorf("parseBoolEnv(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "bogus"} {
		if parseBoolEnv(v) {
			t.Errorf("parseBoolEnv(%q) = true, want false", v)
		}
	}
}

// TestValidateURL_R2M4_RejectsCGNAT is the R2M4 regression guard.
// RFC 6598 CGNAT space (100.64.0.0/10) is NOT covered by Go's
// net.IP.IsPrivate() — it recognizes only RFC 1918 / RFC 4193. A
// webhook server behind a carrier-grade NAT (rare but non-zero in
// cloud deployments) might sit in this range. We extend the SSRF
// guard to treat CGNAT as internal-by-default.
//
// Boundary sample: 100.64.0.0 (first), 100.127.255.254 (near last),
// and a couple of in-range addresses. Negative case: 100.63.0.1 and
// 100.128.0.1 MUST still be allowed — they're outside the /10.
func TestValidateURL_R2M4_RejectsCGNAT(t *testing.T) {
	t.Setenv("RX_ALLOW_INTERNAL_HOOKS", "")
	cgnat := []string{
		"http://100.64.0.0/hook",
		"http://100.64.1.2/hook",
		"http://100.100.100.100/hook",
		"http://100.127.255.254/hook",
	}
	for _, u := range cgnat {
		u := u
		t.Run("reject_"+u, func(t *testing.T) {
			err := ValidateURL(u)
			if err == nil {
				t.Errorf("ValidateURL(%q) accepted a CGNAT URL — SSRF risk.", u)
			}
			if err != nil && !errors.Is(err, ErrInvalidHookURL) {
				t.Errorf("err=%v, want ErrInvalidHookURL", err)
			}
		})
	}
	// Outside-the-range addresses must still be allowed.
	public := []string{
		"http://100.63.255.255/hook", // last /10 pre-range
		"http://100.128.0.0/hook",    // first post-range
		"http://99.99.99.99/hook",
		"http://8.8.8.8/hook",
	}
	for _, u := range public {
		u := u
		t.Run("accept_"+u, func(t *testing.T) {
			if err := ValidateURL(u); err != nil {
				t.Errorf("ValidateURL(%q) rejected a public address as CGNAT: %v", u, err)
			}
		})
	}
}

// TestValidateURL_R2M5_OptionX_DNSResolvesToInternal is the Option X
// test: ValidateURL must resolve DNS names at validation time and
// reject if any resolved IP lies in the internal space. This catches
// the "evil.example.com → 127.0.0.1" class of SSRF, which a purely
// static host-check cannot.
//
// Caveat: this does NOT defend against DNS rebinding (where the
// attacker flips DNS after validation and before the actual POST).
// Rebinding mitigation is Option Y and is deferred to a follow-up
// round. This test only proves the static-DNS-poisoning case is
// closed.
//
// The resolver is injected via the package-level resolveHostFunc so
// the test doesn't depend on real DNS or network state.
func TestValidateURL_R2M5_OptionX_DNSResolvesToInternal(t *testing.T) {
	t.Setenv("RX_ALLOW_INTERNAL_HOOKS", "")
	t.Setenv("RX_HOOK_STRICT_IP_ONLY", "")

	// Inject a resolver that maps "evil.example.com" → 127.0.0.1
	// and "legit.example.com" → 8.8.8.8. Restore on cleanup.
	prev := resolveHostFunc
	resolveHostFunc = func(_ context.Context, host string) ([]net.IP, error) {
		switch host {
		case "evil.example.com":
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		case "mixed.example.com":
			return []net.IP{
				net.ParseIP("8.8.8.8"),
				net.ParseIP("10.0.0.1"), // even one internal IP → reject
			}, nil
		case "legit.example.com":
			return []net.IP{net.ParseIP("8.8.8.8")}, nil
		case "cgnat.example.com":
			return []net.IP{net.ParseIP("100.64.0.1")}, nil
		case "resolve-fail.example.com":
			return nil, errors.New("no such host")
		}
		return nil, errors.New("unknown host in test resolver: " + host)
	}
	t.Cleanup(func() { resolveHostFunc = prev })

	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"evil_resolves_to_loopback", "http://evil.example.com/hook", true},
		{"mixed_one_internal_rejects", "http://mixed.example.com/hook", true},
		{"cgnat_resolution", "http://cgnat.example.com/hook", true},
		{"legit_public_passes", "http://legit.example.com/hook", false},
		// Resolution failure is soft-ACCEPT: if we can't resolve, we
		// can't prove it's internal. Log/let-through is operator-
		// friendly (better than false-positive rejection on a DNS
		// blip). The test asserts non-error behavior here.
		{"resolution_fail_soft_accept", "http://resolve-fail.example.com/hook", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateURL(tc.url)
			gotErr := err != nil
			if gotErr != tc.wantErr {
				t.Errorf("ValidateURL(%q): err=%v, wantErr=%v", tc.url, err, tc.wantErr)
			}
			if gotErr && !errors.Is(err, ErrInvalidHookURL) {
				t.Errorf("err=%v, want ErrInvalidHookURL", err)
			}
		})
	}
}

// TestValidateURL_R2M5_OptionZ_StrictIPOnly is the Option Z test:
// when RX_HOOK_STRICT_IP_ONLY=true, any URL whose host is a DNS name
// (not an IP literal) is rejected outright. This closes the DNS-
// rebinding class by forcing operators to be explicit about the
// target address. Paranoid-mode operators opt in; default users get
// the lighter Option X behavior.
//
// Parity note: when this flag is on, even normally-valid public
// hostnames like api.honestcorp.io are rejected. That's the point —
// operators who enable this flag accept the operational cost of
// maintaining IP allowlists in exchange for rebinding immunity.
func TestValidateURL_R2M5_OptionZ_StrictIPOnly(t *testing.T) {
	t.Setenv("RX_ALLOW_INTERNAL_HOOKS", "")
	t.Setenv("RX_HOOK_STRICT_IP_ONLY", "true")

	// With strict-IP on, hostname URLs are rejected without calling
	// the resolver. Install a tripwire resolver that fails the test
	// if anything tries to use it.
	prev := resolveHostFunc
	resolveHostFunc = func(_ context.Context, host string) ([]net.IP, error) {
		t.Errorf("resolver called for %q under strict-IP; should have short-circuited", host)
		return nil, errors.New("should not be called")
	}
	t.Cleanup(func() { resolveHostFunc = prev })

	reject := []string{
		"http://api.honestcorp.io/hook",
		"https://example.com/hook",
		"http://subdomain.trusted.dev/hook",
	}
	for _, u := range reject {
		u := u
		t.Run("reject_hostname_"+u, func(t *testing.T) {
			err := ValidateURL(u)
			if err == nil {
				t.Errorf("strict-IP mode accepted hostname URL %q", u)
			}
			if err != nil && !errors.Is(err, ErrInvalidHookURL) {
				t.Errorf("err=%v, want ErrInvalidHookURL", err)
			}
		})
	}
	// IP-literal URLs must still pass the hostname check under strict-IP
	// (though they still face the internal-address guard).
	accept := []string{
		"http://8.8.8.8/hook",
		"http://203.0.113.45/hook",
		"http://[2001:db8::1]/hook",
	}
	for _, u := range accept {
		u := u
		t.Run("accept_ip_literal_"+u, func(t *testing.T) {
			if err := ValidateURL(u); err != nil {
				t.Errorf("strict-IP mode rejected IP literal %q: %v", u, err)
			}
		})
	}
}

// TestValidateURL_R2M5_InternalIPLiteralStillRejectedUnderStrictIP
// sanity-checks that turning on strict-IP does NOT bypass the
// internal-address guard: an IP literal in the 127/8 / 10/8 / etc.
// range must still be rejected even when the URL technically passes
// the "is-an-IP" check.
func TestValidateURL_R2M5_InternalIPLiteralStillRejectedUnderStrictIP(t *testing.T) {
	t.Setenv("RX_ALLOW_INTERNAL_HOOKS", "")
	t.Setenv("RX_HOOK_STRICT_IP_ONLY", "true")

	internal := []string{
		"http://127.0.0.1/hook",
		"http://10.0.0.1/hook",
		"http://169.254.169.254/hook",
		"http://100.64.1.1/hook",
	}
	for _, u := range internal {
		u := u
		t.Run(u, func(t *testing.T) {
			err := ValidateURL(u)
			if err == nil {
				t.Errorf("strict-IP mode ALLOWED internal IP %q — guard broke", u)
			}
		})
	}
}
