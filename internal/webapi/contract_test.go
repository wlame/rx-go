package webapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// TestHealth_ReportsContractVersion asserts /health advertises which wire
// contract this backend speaks. The viewer reads it and refuses a major it
// does not know, so it must be present and parseable.
func TestHealth_ReportsContractVersion(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	version, ok := body["contract_version"].(string)
	if !ok || version == "" {
		t.Fatalf("contract_version missing from /health: %v", body)
	}
	parts := strings.Split(version, ".")
	if len(parts) != 2 {
		t.Fatalf("contract_version %q should be MAJOR.MINOR", version)
	}
	for _, p := range parts {
		if _, err := strconv.Atoi(p); err != nil {
			t.Errorf("contract_version %q has a non-numeric component: %v", version, err)
		}
	}
}

// TestContractVersion_IsTheDeclaredConstant keeps the response and the
// constant from drifting apart.
func TestContractVersion_IsTheDeclaredConstant(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)

	if got := body["contract_version"]; got != ContractVersion {
		t.Errorf("contract_version: got %v, want %s", got, ContractVersion)
	}
}

// Contract 2.0 removed the index analysis and GET /v1/detectors, so
// /health reports major 2: a viewer that knows only major 1 refuses
// this server rather than reading fields it no longer sends. Contract
// 2.1 added the journal section of the index answers and the tree's
// not_text_reason, so the minor is 1.
func TestHealth_ReportsContract2(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got := body["contract_version"]; got != "2.1" {
		t.Errorf("contract_version %v, want 2.1", got)
	}
}
