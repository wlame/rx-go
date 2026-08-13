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
