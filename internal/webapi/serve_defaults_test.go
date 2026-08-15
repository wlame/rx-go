package webapi

import "testing"

// The `rx serve` defaults are part of the wire contract. A user who swaps
// this backend for rx-python must not have to change the URL they were
// using, so both are pinned here and in rx-python's
// tests/test_serve_defaults.py. Changing either is a breaking change for
// saved bookmarks, scripts and the viewer's settings.
func TestApplyConfigDefaults_BindAddressMatchesThePythonBackend(t *testing.T) {
	var cfg Config
	applyConfigDefaults(&cfg)

	if cfg.Host != "127.0.0.1" {
		t.Errorf("default host = %q, want 127.0.0.1", cfg.Host)
	}
	if cfg.Port != 7777 {
		t.Errorf("default port = %d, want 7777", cfg.Port)
	}
}

// An explicit value must survive the defaulting pass — the defaults fill
// gaps, they do not override.
func TestApplyConfigDefaults_KeepsAnExplicitBindAddress(t *testing.T) {
	cfg := Config{Host: "0.0.0.0", Port: 8080}
	applyConfigDefaults(&cfg)

	if cfg.Host != "0.0.0.0" || cfg.Port != 8080 {
		t.Errorf("got %s:%d, want 0.0.0.0:8080", cfg.Host, cfg.Port)
	}
}
