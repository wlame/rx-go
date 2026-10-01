package config

import (
	"testing"
	"time"
)

// RX_SAMPLES_WAIT_SECONDS sets how long GET /v1/samples waits for an
// index build; 0 is a valid choice (answer at once), and anything that
// is not a whole number of seconds from 0 up keeps the default.
func TestSamplesIndexWait_FromTheEnvironment(t *testing.T) {
	cases := []struct {
		value string
		want  time.Duration
	}{
		{"", DefaultSamplesIndexWait},
		{"12", 12 * time.Second},
		{"0", 0},
		{"-3", DefaultSamplesIndexWait},
		{"soon", DefaultSamplesIndexWait},
	}
	for _, tc := range cases {
		t.Run("value "+tc.value, func(t *testing.T) {
			t.Setenv("RX_SAMPLES_WAIT_SECONDS", tc.value)
			if got := SamplesIndexWait(); got != tc.want {
				t.Fatalf("SamplesIndexWait() = %v, want %v", got, tc.want)
			}
		})
	}
}
