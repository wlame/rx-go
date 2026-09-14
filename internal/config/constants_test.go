package config

import "testing"

func TestConstants_DefaultValues(t *testing.T) {
	// These constants are user-visible (they appear in /health response).
	// Changing one without user buy-in is a breaking change.
	cases := []struct {
		name string
		got  int
		want int
	}{
		{"MaxSubprocesses default", DefaultMaxSubprocesses, 20},
		{"MinChunkSizeMB default", DefaultMinChunkSizeMB, 20},
		{"LargeFileMB default", DefaultLargeFileMB, 50},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %d, want %d", tc.got, tc.want)
			}
		})
	}
}

func TestConstantsGetters_UseDefaultsWhenUnset(t *testing.T) {
	t.Setenv("RX_MAX_SUBPROCESSES", "")
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "")
	t.Setenv("RX_LARGE_FILE_MB", "")

	cases := []struct {
		name string
		got  int
		want int
	}{
		{"MaxSubprocesses", MaxSubprocesses(), DefaultMaxSubprocesses},
		{"MinChunkSizeMB", MinChunkSizeMB(), DefaultMinChunkSizeMB},
		{"LargeFileMB", LargeFileMB(), DefaultLargeFileMB},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %d, want %d", tc.got, tc.want)
			}
		})
	}
}

func TestConstantsGetters_HonorEnvOverride(t *testing.T) {
	t.Setenv("RX_MAX_SUBPROCESSES", "8")
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "10")
	t.Setenv("RX_LARGE_FILE_MB", "100")

	cases := []struct {
		name string
		got  int
		want int
	}{
		{"MaxSubprocesses override", MaxSubprocesses(), 8},
		{"MinChunkSizeMB override", MinChunkSizeMB(), 10},
		{"LargeFileMB override", LargeFileMB(), 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %d, want %d", tc.got, tc.want)
			}
		})
	}
}
