package index

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureIndexLog sends slog's default logger to a buffer for the rest
// of the test.
func captureIndexLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// storedIndexFixture writes a small log, builds and stores its index,
// and returns the log's path and the index file's path.
func storedIndexFixture(t *testing.T) (source, cachePath string) {
	t.Helper()
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	source = filepath.Join(t.TempDir(), "app.log")
	writePlain(t, source, numberedText(200, "LINE"))
	built, err := Build(source, BuildOptions{StepBytes: 512})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	cachePath, err = Save(built)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	return source, cachePath
}

// rewriteTimeIndex gives the stored index at cachePath a time section,
// changed by edit, which also learns the line number of each of the
// index's checkpoints, in order. The rest of the index stays valid.
func rewriteTimeIndex(t *testing.T, cachePath string, edit func(ti map[string]any, checkpoints []int64)) {
	t.Helper()
	body, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse index: %v", err)
	}
	var checkpoints []int64
	for _, entry := range doc["line_index"].([]any) {
		checkpoints = append(checkpoints, int64(entry.([]any)[0].(float64)))
	}
	ti := map[string]any{
		"format": "iso", "anchored": true, "day_first": nil, "has_zone": false,
		"year_from_mtime": false, "timestamped_lines": 0, "first": nil, "last": nil, "max": nil,
		"first_zone_offset_minutes": nil, "backward_steps": 0, "max_backward_ms": 0,
		"max_before": make([]any, len(checkpoints)), "first_text": nil, "zone_offsets": []any{},
	}
	edit(ti, checkpoints)
	doc["time_index"] = ti
	if body, err = json.Marshal(doc); err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(cachePath, body, 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
}

// lineOffset is the byte offset at which line n starts in the text
// numberedText(lines, "LINE") writes, whatever lines is: the fixtures'
// text. Line 1, and a line number below it, start at 0.
func lineOffset(n int64) int64 {
	var offset int64
	for i := int64(1); i < n; i++ {
		offset += int64(len(fmt.Sprintf("LINE %d LINE\n", i)))
	}
	return offset
}

// storedPoint is a time point of the fixtures' text: value ms on line line,
// at the offset where that line starts, so the offsets of several
// points keep the order of their lines as a build records them.
func storedPoint(ms, line int64) map[string]any {
	return map[string]any{"ms": ms, "line": line, "offset": lineOffset(line)}
}

// maxBeforeOf is the max_before a build records for a file whose
// highest value, highest, is first held by line maxLine, when no line
// before that one has a value: null at each checkpoint up to that line,
// and highest at each one after it.
func maxBeforeOf(checkpoints []int64, maxLine, highest int64) []any {
	values := make([]any, len(checkpoints))
	for i, line := range checkpoints {
		if line > maxLine {
			values[i] = highest
		}
	}
	return values
}

// An index file that cannot be read or parsed — cut short by a power
// loss or a full disk, or left unreadable by its permissions — is not
// an index. It is reported as absent, the way a missing one is, so a
// lookup answers without it and a build replaces it; one warning names
// the file so the operator hears about it.
func TestLoadFromPath_DamagedIndexIsAbsentAndLogged(t *testing.T) {
	damages := []struct {
		name   string
		damage func(t *testing.T, cachePath string)
	}{
		{"truncated", func(t *testing.T, cachePath string) {
			body, err := os.ReadFile(cachePath)
			if err != nil {
				t.Fatalf("read index: %v", err)
			}
			if err := os.WriteFile(cachePath, body[:len(body)/2], 0o600); err != nil {
				t.Fatalf("truncate index: %v", err)
			}
		}},
		{"unreadable", func(t *testing.T, cachePath string) {
			if os.Geteuid() == 0 {
				t.Skip("root reads a file whatever its permissions")
			}
			if err := os.Chmod(cachePath, 0); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			t.Cleanup(func() { _ = os.Chmod(cachePath, 0o600) })
		}},
		// A time section a search cannot trust: max_before must have one
		// entry per checkpoint, never decrease, and name a known format.
		{"time section misaligned with the checkpoints", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, checkpoints []int64) {
				ti["max_before"] = make([]any, len(checkpoints)+1)
			})
		}},
		{"time section whose maximum decreases", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, checkpoints []int64) {
				setFirstAndLast(ti)
				ti["first_text"] = "2025-12-10 07:00:04.574"
				ti["max"] = storedPoint(2_000_000, 100)
				values := make([]any, len(checkpoints))
				for i := range values {
					values[i] = 1_000_000 - i
				}
				ti["max_before"] = values
			})
		}},
		{"time section of an unknown format", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, checkpoints []int64) {
				ti["format"] = "stardate"
				ti["max_before"] = make([]any, len(checkpoints))
			})
		}},
		// The fixture has 200 lines; first, last and the count of
		// timestamped lines must describe lines it has.
		{"time section with a negative count of timestamped lines", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				ti["timestamped_lines"] = -1
			})
		}},
		{"time section without first and last for its timestamped lines", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				ti["timestamped_lines"] = 3
			})
		}},
		{"time section whose first line is line 0", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				ti["timestamped_lines"] = 3
				ti["first"] = storedPoint(1, 0)
				ti["last"] = storedPoint(2, 200)
				ti["max"] = storedPoint(2, 200)
			})
		}},
		{"time section whose last line is past the last line", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				ti["timestamped_lines"] = 3
				ti["first"] = storedPoint(1, 1)
				ti["last"] = storedPoint(2, 201)
				ti["max"] = storedPoint(2, 200)
			})
		}},
		// The text of the first timestamp is shown to a client as it is:
		// it must be there for a first line, short, and printable.
		{"time section without the text of its first timestamp", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setFirstAndLast(ti)
			})
		}},
		{"time section whose first text is too long", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setFirstAndLast(ti)
				ti["first_text"] = strings.Repeat("9", 65)
			})
		}},
		{"time section whose first text holds a control byte", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setFirstAndLast(ti)
				ti["first_text"] = "2025-12-10\x1b[31m"
			})
		}},
		{"time section with a first text and no first line", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				ti["first_text"] = "2025-12-10 07:00:04.574"
			})
		}},
		// A stored value is a moment in the years 1 to 9999; a search adds
		// up to 18 hours to it, which must not leave the int64 range.
		{"time section whose first value is after the year 9999", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setFirstAndLast(ti)
				ti["first_text"] = "2025-12-10 07:00:04.574"
				ti["first"] = storedPoint(int64(math.MaxInt64), 1)
			})
		}},
		{"time section whose last value is before the year 1", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setFirstAndLast(ti)
				ti["first_text"] = "2025-12-10 07:00:04.574"
				ti["last"] = storedPoint(int64(-62135596800001), 200)
			})
		}},
		// max is the highest value of the file, which a search by time
		// across several files trusts to pass over a file whose lines all
		// come before the time it looks for.
		{"time section without max for its timestamped lines", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setFirstAndLast(ti)
				ti["first_text"] = "2025-12-10 07:00:04.574"
				ti["max"] = nil
			})
		}},
		{"time section with max and no timestamped lines", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				ti["max"] = storedPoint(2, 200)
			})
		}},
		{"time section whose max is before its first line", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setZonedSpan(ti, []any{[]any{10, 120}})
				ti["max"] = storedPoint(2, 9)
			})
		}},
		{"time section whose max is after its last line", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setZonedSpan(ti, []any{[]any{10, 120}})
				ti["last"] = storedPoint(2, 150)
				ti["max"] = storedPoint(2, 151)
			})
		}},
		{"time section whose max is below its last value", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setFirstAndLast(ti)
				ti["first_text"] = "2025-12-10 07:00:04.574"
				ti["max"] = storedPoint(1, 100)
			})
		}},
		{"time section whose max is below its first value", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setFirstAndLast(ti)
				ti["first_text"] = "2025-12-10 07:00:04.574"
				ti["first"] = storedPoint(3, 1)
			})
		}},
		{"time section whose max is below a max_before entry", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, checkpoints []int64) {
				setFirstAndLast(ti)
				ti["first_text"] = "2025-12-10 07:00:04.574"
				values := make([]any, len(checkpoints))
				values[len(checkpoints)-1] = 3
				ti["max_before"] = values
			})
		}},
		{"time section whose max is after the year 9999", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setFirstAndLast(ti)
				ti["first_text"] = "2025-12-10 07:00:04.574"
				ti["max"] = storedPoint(int64(253402300800000), 200)
			})
		}},
		{"time section whose max_before is after the year 9999", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, checkpoints []int64) {
				values := make([]any, len(checkpoints))
				values[len(checkpoints)-1] = int64(253402300800000)
				ti["max_before"] = values
			})
		}},
		{"time section with a zone offset beyond 18 hours", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				ti["has_zone"] = true
				ti["first_zone_offset_minutes"] = 18*60 + 1
			})
		}},
		// zone_offsets turns a stored value into the wall clock its line
		// writes, segment by segment, so a search trusts its every entry.
		{"zone offsets that are not in line order", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setZonedSpan(ti, []any{[]any{10, 120}, []any{90, 60}, []any{50, 120}})
			})
		}},
		{"zone offsets naming one line twice", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setZonedSpan(ti, []any{[]any{10, 120}, []any{90, 60}, []any{90, 120}})
			})
		}},
		{"zone offsets naming a line after the last timestamped line", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setZonedSpan(ti, []any{[]any{10, 120}, []any{201, 60}})
			})
		}},
		{"zone offsets that do not start at the first timestamped line", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setZonedSpan(ti, []any{[]any{1, 120}, []any{90, 60}})
			})
		}},
		{"zone offsets whose first offset is not the first timestamp's", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setZonedSpan(ti, []any{[]any{10, 60}})
			})
		}},
		{"zone offsets with an offset beyond 18 hours", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setZonedSpan(ti, []any{[]any{10, 120}, []any{90, -18*60 - 1}})
			})
		}},
		{"zone offsets repeating the offset in force", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setZonedSpan(ti, []any{[]any{10, 120}, []any{90, 120}})
			})
		}},
		{"zone offsets with a third element", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setZonedSpan(ti, []any{[]any{10, 120, 7}})
			})
		}},
		{"zone offsets empty for timestamped lines", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setZonedSpan(ti, []any{})
			})
		}},
		{"zone offsets for a file without timestamped lines", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				ti["zone_offsets"] = []any{[]any{1, 0}}
			})
		}},
		{"zone offsets null for a file whose timestamps carry no zone", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setFirstAndLast(ti)
				ti["first_text"] = "2025-12-10 07:00:04.574"
				ti["zone_offsets"] = nil
			})
		}},
		{"zone offsets other than one zero for a file whose timestamps carry no zone", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setFirstAndLast(ti)
				ti["first_text"] = "2025-12-10 07:00:04.574"
				ti["zone_offsets"] = []any{[]any{1, 0}, []any{90, -420}}
			})
		}},
		// first, max and last are positions in one text, so their
		// offsets keep the order of their lines, and only one line has a
		// given offset. A reader that seeks to a stored offset trusts it.
		{"time section whose first offset is negative", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setFirstAndLast(ti)
				ti["first_text"] = "2025-12-10 07:00:04.574"
				ti["first"] = map[string]any{"ms": 1, "line": 1, "offset": -1}
			})
		}},
		{"time section whose max offset is before its first offset", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, checkpoints []int64) {
				setFirstAndLast(ti)
				ti["first_text"] = "2025-12-10 07:00:04.574"
				ti["first"] = storedPoint(1, 50)
				ti["zone_offsets"] = []any{[]any{50, 0}}
				ti["max"] = map[string]any{"ms": 2, "line": 100, "offset": lineOffset(40)}
				ti["max_before"] = maxBeforeOf(checkpoints, 100, 2)
			})
		}},
		{"time section whose max offset is after its last offset", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, checkpoints []int64) {
				setFirstAndLast(ti)
				ti["first_text"] = "2025-12-10 07:00:04.574"
				ti["max"] = map[string]any{"ms": 2, "line": 100, "offset": lineOffset(200) + 1}
				ti["max_before"] = maxBeforeOf(checkpoints, 100, 2)
			})
		}},
		{"time section whose max and last share an offset on two lines", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, checkpoints []int64) {
				setFirstAndLast(ti)
				ti["first_text"] = "2025-12-10 07:00:04.574"
				ti["max"] = map[string]any{"ms": 2, "line": 100, "offset": lineOffset(200)}
				ti["max_before"] = maxBeforeOf(checkpoints, 100, 2)
			})
		}},
		{"time section whose max and last name one line at two offsets", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setFirstAndLast(ti)
				ti["first_text"] = "2025-12-10 07:00:04.574"
				ti["max"] = map[string]any{"ms": 2, "line": 200, "offset": lineOffset(199)}
			})
		}},
		// max names the first line that holds the highest value, so the
		// highest value before a checkpoint after that line is max's,
		// and before a checkpoint up to that line it is lower or none.
		{"time section whose max_before after max's line is not max", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, checkpoints []int64) {
				setFirstAndLast(ti)
				ti["first_text"] = "2025-12-10 07:00:04.574"
				ti["max"] = storedPoint(2, 100)
				ti["max_before"] = maxBeforeOf(checkpoints, 100, 1)
			})
		}},
		{"time section whose max_before after max's line is null", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, _ []int64) {
				setFirstAndLast(ti)
				ti["first_text"] = "2025-12-10 07:00:04.574"
				ti["max"] = storedPoint(2, 100)
			})
		}},
		{"time section whose max_before up to max's line equals max", func(t *testing.T, cachePath string) {
			rewriteTimeIndex(t, cachePath, func(ti map[string]any, checkpoints []int64) {
				setFirstAndLast(ti)
				ti["first_text"] = "2025-12-10 07:00:04.574"
				values := make([]any, len(checkpoints))
				values[len(checkpoints)-1] = 2
				ti["max_before"] = values
			})
		}},
	}
	for _, tc := range damages {
		t.Run(tc.name, func(t *testing.T) {
			source, cachePath := storedIndexFixture(t)
			tc.damage(t, cachePath)
			log := captureIndexLog(t)

			idx, err := LoadFromPath(cachePath)
			if idx != nil || !errors.Is(err, ErrIndexNotFound) {
				t.Errorf("LoadFromPath = %v, %v; want no index and ErrIndexNotFound", idx, err)
			}
			if idx, err := LoadForSource(source); idx != nil || !errors.Is(err, ErrIndexNotFound) {
				t.Errorf("LoadForSource = %v, %v; want no index and ErrIndexNotFound", idx, err)
			}
			logged := log.String()
			if !strings.Contains(logged, "index_unreadable") || !strings.Contains(logged, filepath.Base(cachePath)) {
				t.Errorf("no warning naming the damaged index; log:\n%s", logged)
			}
			// Each row of a time section is refused for its own fault,
			// not for another one its fixture happens to have.
			if want, ok := damagedTimeSectionReasons[tc.name]; ok && !strings.Contains(logged, want) {
				t.Errorf("refused, but not for %q; log:\n%s", want, logged)
			}
			if strings.HasPrefix(tc.name, "time section") || strings.HasPrefix(tc.name, "zone offsets") {
				if _, ok := damagedTimeSectionReasons[tc.name]; !ok {
					t.Errorf("no expected reason for %q", tc.name)
				}
			}
		})
	}
}

// damagedTimeSectionReasons is the text the load check gives for each
// damaged time section of TestLoadFromPath_DamagedIndexIsAbsentAndLogged.
var damagedTimeSectionReasons = map[string]string{
	"time section misaligned with the checkpoints":                               "max_before has",
	"time section whose maximum decreases":                                       "max_before decreases",
	"time section of an unknown format":                                          "stardate",
	"time section with a negative count of timestamped lines":                    "timestamped_lines is -1",
	"time section without first and last for its timestamped lines":              "first, last and max do not match",
	"time section whose first line is line 0":                                    "first names line 0",
	"time section whose last line is past the last line":                         "last names line 201",
	"time section without the text of its first timestamp":                       "first_text does not match first",
	"time section whose first text is too long":                                  "first_text is 65 bytes",
	"time section whose first text holds a control byte":                         "not printable ASCII",
	"time section with a first text and no first line":                           "first_text does not match first",
	"time section whose first value is after the year 9999":                      "first holds",
	"time section whose last value is before the year 1":                         "last holds",
	"time section without max for its timestamped lines":                         "first, last and max do not match",
	"time section with max and no timestamped lines":                             "first, last and max do not match",
	"time section whose max is before its first line":                            "max names line 9, outside",
	"time section whose max is after its last line":                              "max names line 151, outside",
	"time section whose max is below its last value":                             "below first's",
	"time section whose max is below its first value":                            "below first's",
	"time section whose max is below a max_before entry":                         "not below max",
	"time section whose max is after the year 9999":                              "max holds",
	"time section whose max_before is after the year 9999":                       "outside the years 1 to 9999",
	"time section with a zone offset beyond 18 hours":                            "first_zone_offset_minutes",
	"zone offsets that are not in line order":                                    "does not follow",
	"zone offsets naming one line twice":                                         "does not follow",
	"zone offsets naming a line after the last timestamped line":                 "after the last timestamped line",
	"zone offsets that do not start at the first timestamped line":               "not at the first timestamp",
	"zone offsets whose first offset is not the first timestamp's":               "not at the first timestamp",
	"zone offsets with an offset beyond 18 hours":                                "is beyond 18 hours",
	"zone offsets repeating the offset in force":                                 "does not follow",
	"zone offsets with a third element":                                          "unmarshal",
	"zone offsets empty for timestamped lines":                                   "zone_offsets has 0 entries",
	"zone offsets for a file without timestamped lines":                          "zone_offsets has 1 entries",
	"zone offsets null for a file whose timestamps carry no zone":                "zone_offsets is null",
	"zone offsets other than one zero for a file whose timestamps carry no zone": "carry no zone",
	"time section whose first offset is negative":                                "first offset -1 is negative",
	"time section whose max offset is before its first offset":                   "offsets are not in the order of their lines",
	"time section whose max offset is after its last offset":                     "offsets are not in the order of their lines",
	"time section whose max and last share an offset on two lines":               "offsets are not in the order of their lines",
	"time section whose max and last name one line at two offsets":               "offsets are not in the order of their lines",
	"time section whose max_before after max's line is not max":                  "not max",
	"time section whose max_before after max's line is null":                     "not max",
	"time section whose max_before up to max's line equals max":                  "not below max",
}

// zone_offsets longer than the limit is refused before its entries are
// read one by one. The fixture has enough lines for every entry to name
// a line of its own, so the length is the only fault.
func TestLoadFromPath_RefusesZoneOffsetsPastTheLimit(t *testing.T) {
	t.Setenv("RX_CACHE_DIR", t.TempDir())
	source := filepath.Join(t.TempDir(), "app.log")
	writePlain(t, source, numberedText(MaxZoneOffsets+100, "LINE"))
	built, err := Build(source, BuildOptions{StepBytes: 4096})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	cachePath, err := Save(built)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	for _, entries := range []int{MaxZoneOffsets, MaxZoneOffsets + 1} {
		rewriteTimeIndex(t, cachePath, func(ti map[string]any, checkpoints []int64) {
			points := []any{}
			for i := 0; i < entries; i++ {
				points = append(points, []any{1 + i, 60 + 60*(i%2)})
			}
			setZonedSpan(ti, points)
			ti["first"] = storedPoint(1, 1)
			ti["last"] = storedPoint(2, int64(entries))
			ti["max"] = storedPoint(2, int64(entries))
			ti["max_before"] = maxBeforeOf(checkpoints, int64(entries), 2)
			ti["timestamped_lines"] = entries
			ti["first_zone_offset_minutes"] = 60
		})
		idx, err := LoadFromPath(cachePath)
		if loads := idx != nil; loads != (entries <= MaxZoneOffsets) {
			t.Errorf("%d entries: LoadFromPath = %v, %v", entries, idx, err)
		}
	}
}

// setFirstAndLast gives a time section three timestamped lines, with
// a first and a last line the fixture has, the highest value on the last
// line, and the one zone offset of a file whose timestamps carry no zone.
func setFirstAndLast(ti map[string]any) {
	ti["timestamped_lines"] = 3
	ti["first"] = storedPoint(1, 1)
	ti["last"] = storedPoint(2, 200)
	ti["max"] = storedPoint(2, 200)
	ti["zone_offsets"] = []any{[]any{1, 0}}
}

// setZonedSpan gives a time section of a file whose timestamps carry
// zones: first on line 10, last on the file's last line (200) with the
// highest value, the first written at +02:00, the text of the first
// present, and zone_offsets as given.
func setZonedSpan(ti map[string]any, zoneOffsets any) {
	setFirstAndLast(ti)
	ti["has_zone"] = true
	ti["first"] = storedPoint(1, 10)
	ti["last"] = storedPoint(2, 200)
	ti["max"] = storedPoint(2, 200)
	ti["first_zone_offset_minutes"] = 120
	ti["first_text"] = "2025-10-26T02:00:00.000+02:00"
	ti["zone_offsets"] = zoneOffsets
}

// A time section at the edges of what the checks accept still loads:
// first on line 1, last on the file's last line, max equal to last's
// value and to a max_before entry (on line 2, the first line to hold
// it), a zone 18 hours west, a first text of the longest length, zone
// offset changes up to 18 hours either way and on the last timestamped
// line, and values at the first and last millisecond of the years 1 to
// 9999.
func TestLoadFromPath_TimeSectionAtTheEdgesLoads(t *testing.T) {
	_, cachePath := storedIndexFixture(t)
	rewriteTimeIndex(t, cachePath, func(ti map[string]any, checkpoints []int64) {
		ti["has_zone"] = true
		ti["timestamped_lines"] = 200
		ti["first"] = storedPoint(int64(-62135596800000), 1)
		ti["last"] = storedPoint(int64(253402300799999), 200)
		ti["max"] = storedPoint(int64(253402300799999), 2)
		ti["max_before"] = maxBeforeOf(checkpoints, 2, int64(253402300799999))
		ti["first_zone_offset_minutes"] = -18 * 60
		ti["first_text"] = strings.Repeat("9", 64)
		ti["zone_offsets"] = []any{[]any{1, -18 * 60}, []any{100, 18 * 60}, []any{200, 0}}
	})
	if idx, err := LoadFromPath(cachePath); idx == nil || idx.TimeIndex == nil {
		t.Fatalf("LoadFromPath = %v, %v; want the index with its time section", idx, err)
	}
}

// A missing index and one of another format version are ordinary
// misses: absent, and not worth a warning.
func TestLoadFromPath_OrdinaryMissIsNotLogged(t *testing.T) {
	misses := []struct {
		name  string
		setUp func(t *testing.T, cachePath string)
	}{
		{"missing", func(t *testing.T, cachePath string) {
			if err := os.Remove(cachePath); err != nil {
				t.Fatalf("remove index: %v", err)
			}
		}},
		{"other version", func(t *testing.T, cachePath string) {
			body, err := json.Marshal(map[string]any{"version": Version - 1})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := os.WriteFile(cachePath, body, 0o600); err != nil {
				t.Fatalf("write index: %v", err)
			}
		}},
	}
	for _, tc := range misses {
		t.Run(tc.name, func(t *testing.T) {
			_, cachePath := storedIndexFixture(t)
			tc.setUp(t, cachePath)
			log := captureIndexLog(t)

			if idx, err := LoadFromPath(cachePath); idx != nil || !errors.Is(err, ErrIndexNotFound) {
				t.Errorf("LoadFromPath = %v, %v; want no index and ErrIndexNotFound", idx, err)
			}
			if strings.Contains(log.String(), "index_unreadable") {
				t.Errorf("an ordinary miss was logged as unreadable:\n%s", log.String())
			}
		})
	}
}
