package samples

import (
	"reflect"
	"testing"
)

// A range of lines counted from the end answers as the range it names
// counted from the start: --lines=-3--1 on a 300-line file is lines
// 298-300, keyed by that range, with the same lines and the same -1 as
// --lines=298-300, in every storage and with or without an index.
func TestResolve_NegativeLineRangeIsTheRangeItNames(t *testing.T) {
	const total = 300
	plain, gz := writeGzipFixture(t, total)
	zst, zstLoader, _ := makeIndexedSeekable(t, total, 4*1024)
	minus3, minus1, from, to := int64(-3), int64(-1), int64(total-2), int64(total)
	negative := []OffsetOrRange{{Start: minus3, End: &minus1}}
	positive := []OffsetOrRange{{Start: from, End: &to}}

	for _, tc := range []struct {
		name   string
		path   string
		loader IndexLoader
	}{
		{"plain", plain, NoIndex},
		{"gzip", gz, NoIndex},
		{"seekable without its frame table", zst, NoIndex},
		{"seekable through its frame table", zst, zstLoader},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(t.Context(), Request{Path: tc.path, Lines: negative, IndexLoader: tc.loader})
			if err != nil {
				t.Fatalf("Resolve(-3--1): %v", err)
			}
			want, err := Resolve(t.Context(), Request{Path: tc.path, Lines: positive, IndexLoader: tc.loader})
			if err != nil {
				t.Fatalf("Resolve(298-300): %v", err)
			}
			if _, ok := got.Samples["298-300"]; !ok {
				t.Fatalf("keys = %v, want the resolved range 298-300", keysOfSamples(got.Samples))
			}
			if !reflect.DeepEqual(got.Samples, want.Samples) || !reflect.DeepEqual(got.Lines, want.Lines) {
				t.Errorf("-3--1 answered\n%v %v\nwant, as 298-300,\n%v %v", got.Samples, got.Lines, want.Samples, want.Lines)
			}
			if len(got.Samples["298-300"]) != 3 {
				t.Errorf("samples[298-300] = %v, want three lines", got.Samples["298-300"])
			}
		})
	}
}

func keysOfSamples(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
