package config

import "testing"

// RX_MASK_SECRETS is off unless set to a truthy value, in the boolean
// forms of RX_HIDDEN and RX_NO_INDEX; a value that is not one of them
// is refused as theirs is: the default, off, is used.
func TestMaskSecrets_ReadsTheBooleanForms(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"", false},
		{"1", true},
		{"true", true},
		{"YES", true},
		{"on", true},
		{"0", false},
		{"false", false},
		{"no", false},
		{"off", false},
		{"maybe", false},
	}
	for _, tc := range cases {
		t.Run("value "+tc.value, func(t *testing.T) {
			t.Setenv("RX_MASK_SECRETS", tc.value)
			if got := MaskSecrets(); got != tc.want {
				t.Errorf("RX_MASK_SECRETS=%q: MaskSecrets() = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}
