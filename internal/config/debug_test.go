package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDebugMode(t *testing.T) {
	cases := []struct {
		set  *string
		want bool
	}{
		{nil, false},
		{strPtr(""), false},
		{strPtr("1"), true},
		{strPtr("true"), true},
		{strPtr("yes"), true},
		{strPtr("0"), false},
		{strPtr("no"), false},
	}
	for i, tc := range cases {
		if tc.set == nil {
			os.Unsetenv("RX_DEBUG")
		} else {
			t.Setenv("RX_DEBUG", *tc.set)
		}
		if got := DebugMode(); got != tc.want {
			t.Errorf("case %d: got %v, want %v", i, got, tc.want)
		}
	}
}

func TestDebugDir_RXDebugDirOverride(t *testing.T) {
	t.Setenv("RX_DEBUG_DIR", "/custom/debug")
	got := DebugDir()
	if got != "/custom/debug" {
		t.Errorf("got %q, want /custom/debug", got)
	}
}

func TestDebugDir_DefaultsToTempDirRxDebug(t *testing.T) {
	os.Unsetenv("RX_DEBUG_DIR")
	got := DebugDir()
	want := filepath.Join(os.TempDir(), "rx-debug")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
