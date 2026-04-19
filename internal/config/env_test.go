package config

import (
	"os"
	"testing"
)

func TestGetIntEnv(t *testing.T) {
	cases := []struct {
		name string
		set  *string // nil = unset
		def  int
		want int
	}{
		{"unset returns default", nil, 42, 42},
		{"empty string returns default", strPtr(""), 42, 42},
		{"valid int", strPtr("7"), 42, 7},
		{"negative int", strPtr("-5"), 42, -5},
		{"zero", strPtr("0"), 42, 0},
		{"invalid returns default", strPtr("not-a-number"), 42, 42},
		{"decimal rejected (Python parity)", strPtr("1.5"), 42, 42},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set == nil {
				t.Setenv("RX_TEST_INT", "")
				os.Unsetenv("RX_TEST_INT")
			} else {
				t.Setenv("RX_TEST_INT", *tc.set)
			}
			got := GetIntEnv("RX_TEST_INT", tc.def)
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestGetStringEnv(t *testing.T) {
	cases := []struct {
		name string
		set  *string
		def  string
		want string
	}{
		{"unset returns default", nil, "default", "default"},
		{"set to empty string returns empty (not default)", strPtr(""), "default", ""},
		{"set to value", strPtr("hello"), "default", "hello"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set == nil {
				os.Unsetenv("RX_TEST_STR")
			} else {
				t.Setenv("RX_TEST_STR", *tc.set)
			}
			got := GetStringEnv("RX_TEST_STR", tc.def)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGetBoolEnv(t *testing.T) {
	cases := []struct {
		name string
		set  *string
		def  bool
		want bool
	}{
		{"unset returns default false", nil, false, false},
		{"unset returns default true", nil, true, true},
		{"true", strPtr("true"), false, true},
		{"TRUE (case insensitive)", strPtr("TRUE"), false, true},
		{"yes", strPtr("yes"), false, true},
		{"1", strPtr("1"), false, true},
		{"on", strPtr("on"), false, true},
		{"false", strPtr("false"), true, false},
		{"FALSE", strPtr("FALSE"), true, false},
		{"no", strPtr("no"), true, false},
		{"0", strPtr("0"), true, false},
		{"off", strPtr("off"), true, false},
		{"garbage returns default", strPtr("maybe"), true, true},
		{"garbage returns default false", strPtr("maybe"), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set == nil {
				os.Unsetenv("RX_TEST_BOOL")
			} else {
				t.Setenv("RX_TEST_BOOL", *tc.set)
			}
			got := GetBoolEnv("RX_TEST_BOOL", tc.def)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGetPathSepEnv(t *testing.T) {
	t.Run("unset returns nil", func(t *testing.T) {
		os.Unsetenv("RX_TEST_PATH")
		if got := GetPathSepEnv("RX_TEST_PATH"); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("single path", func(t *testing.T) {
		t.Setenv("RX_TEST_PATH", "/tmp")
		got := GetPathSepEnv("RX_TEST_PATH")
		want := []string{"/tmp"}
		if !stringSliceEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("multiple paths", func(t *testing.T) {
		t.Setenv("RX_TEST_PATH", "/tmp"+string(os.PathListSeparator)+"/var/log")
		got := GetPathSepEnv("RX_TEST_PATH")
		want := []string{"/tmp", "/var/log"}
		if !stringSliceEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("empty entries skipped", func(t *testing.T) {
		sep := string(os.PathListSeparator)
		t.Setenv("RX_TEST_PATH", "/tmp"+sep+sep+"/var/log"+sep)
		got := GetPathSepEnv("RX_TEST_PATH")
		want := []string{"/tmp", "/var/log"}
		if !stringSliceEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

// strPtr returns a pointer to s — keeps table-driven tests readable.
func strPtr(s string) *string { return &s }

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
