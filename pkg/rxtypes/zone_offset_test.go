package rxtypes

import (
	"encoding/json"
	"testing"
)

// A zone offset change point is written as [line, minutes], and only
// that shape is read back: a pair of whole numbers.
func TestZoneOffset_RoundTripsAsAPair(t *testing.T) {
	t.Parallel()
	in := []ZoneOffset{{Line: 3, OffsetMinutes: 120}, {Line: 900, OffsetMinutes: -420}}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(body) != `[[3,120],[900,-420]]` {
		t.Fatalf("got %s", body)
	}
	var out []ZoneOffset
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(out) != 2 || out[0] != in[0] || out[1] != in[1] {
		t.Errorf("read back %+v, want %+v", out, in)
	}
}

func TestZoneOffset_RefusesAnotherShape(t *testing.T) {
	t.Parallel()
	for _, input := range []string{`[1]`, `[1, 2, 3]`, `[]`, `{"line": 1}`, `"1,2"`, `[1.5, 2]`, `[1, 2.5]`} {
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			var got ZoneOffset
			if err := json.Unmarshal([]byte(input), &got); err == nil {
				t.Errorf("read %s as %+v; want an error", input, got)
			}
		})
	}
}

// A time section keeps the difference between no change points (an
// empty list) and too many to record (null).
func TestTimeIndex_ZoneOffsetsNullAndEmptyStayApart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		json   string
		isNull bool
	}{{`{"zone_offsets":null}`, true}, {`{"zone_offsets":[]}`, false}} {
		var ti TimeIndex
		if err := json.Unmarshal([]byte(tc.json), &ti); err != nil {
			t.Fatalf("Unmarshal(%s): %v", tc.json, err)
		}
		if (ti.ZoneOffsets == nil) != tc.isNull {
			t.Errorf("%s read as %#v", tc.json, ti.ZoneOffsets)
		}
		body, err := json.Marshal(ti)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatalf("parse %s: %v", body, err)
		}
		want := "[]"
		if tc.isNull {
			want = "null"
		}
		if string(doc["zone_offsets"]) != want {
			t.Errorf("%s written back as zone_offsets %s", tc.json, doc["zone_offsets"])
		}
	}
}
