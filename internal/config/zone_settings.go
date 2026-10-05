package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	// The zone database compiled into the binary, so a zone name in
	// RX_LOG_TZ or RX_QUERY_TZ works on a host without /usr/share/zoneinfo
	// (a minimal container image). It adds about 450 KB; Go uses the
	// host's database first when there is one.
	_ "time/tzdata"
)

// ZoneSetting describes one time-zone environment variable: its name,
// the zone used when it is unset or not acceptable, and whether it
// accepts `local` for the process's own zone.
//
// Every zone variable follows one rule, applied by Value:
//
//   - unset or empty: Default, silently;
//   - `UTC`, an IANA zone name (`Europe/Berlin`) or a fixed offset
//     `±HH:MM` up to 18 hours: that zone;
//   - `local`, when AcceptsLocal is set: the process's zone;
//   - anything else: Default, with one invalid_setting warning per
//     variable and value in a process, as the integer settings warn.
type ZoneSetting struct {
	// Name is the environment variable, e.g. "RX_LOG_TZ".
	Name string
	// Default is the zone used when Name is unset or not acceptable, in
	// a form the rule accepts; "" means no zone, which the caller reads
	// as its own default.
	Default string
	// AcceptsLocal makes `local` name the process's zone.
	AcceptsLocal bool
}

// Zone is the value of a ZoneSetting.
type Zone struct {
	// Location is the zone, or nil when the setting names none (unset,
	// with an empty Default).
	Location *time.Location
	// Name is the zone as the setting gave it (`Europe/Berlin`,
	// `+02:00`, `UTC`, `local`), or "" with no zone.
	Name string
}

// localZoneName is the value that names the process's own zone.
const localZoneName = "local"

// maxFixedOffsetHours bounds a `±HH:MM` value: 18 hours, the largest
// offset RFC 3339 readers and Go's time package accept.
const maxFixedOffsetHours = 18

// The zone settings, as data. docs/configuration.md lists the same rows
// ("Zone settings"), and a test keeps the two in step.
var (
	// LogTZSetting is RX_LOG_TZ: the zone a file's timestamps were
	// written in when they carry none.
	LogTZSetting = ZoneSetting{Name: "RX_LOG_TZ", Default: "UTC"}

	// QueryTZSetting is RX_QUERY_TZ: the zone a time query is read in
	// when it carries none. Unset reads such a query in the file's own
	// frame.
	QueryTZSetting = ZoneSetting{Name: "RX_QUERY_TZ", AcceptsLocal: true}
)

// ZoneSettings is every zone setting rx reads, in the order the
// documentation lists them.
var ZoneSettings = []ZoneSetting{LogTZSetting, QueryTZSetting}

// LogTZ returns RX_LOG_TZ, by the rule on ZoneSetting.
func LogTZ() Zone { return LogTZSetting.Value() }

// QueryTZ returns RX_QUERY_TZ, by the rule on ZoneSetting.
func QueryTZ() Zone { return QueryTZSetting.Value() }

// ErrInvalidZone is returned by ParseZone for a value that names no zone
// the rule accepts.
var ErrInvalidZone = errors.New("invalid time zone")

// maxZoneValueBytes bounds a zone value a request gives before it is
// looked up. The longest IANA zone name is 32 bytes
// (America/Argentina/ComodRivadavia); a longer value is refused without
// a look in the zone database.
const maxZoneValueBytes = 64

// zoneValuesAccepted names what ParseZone accepts, for its errors.
const zoneValuesAccepted = "UTC, an IANA zone name or ±HH:MM"

// ParseZone reads a zone a request names, such as `rx samples
// --file-tz=…` or `file_tz=…`: UTC, an IANA zone name or a fixed offset
// ±HH:MM up to 18 hours, by the rule RX_LOG_TZ follows (two digits in
// each field of an offset). `local` is refused: the process's zone is
// not the caller's to name. raw must not be empty; the caller decides
// what an empty value means.
//
// The error wraps ErrInvalidZone and says what is wrong with raw; it
// does not repeat raw, which the caller names in its own words.
func ParseZone(raw string) (Zone, error) {
	if raw == "" || len(raw) > maxZoneValueBytes {
		return Zone{}, fmt.Errorf("%w: not a zone name; give %s", ErrInvalidZone, zoneValuesAccepted)
	}
	zone, err := LogTZSetting.parse(raw)
	if err != nil {
		return Zone{}, fmt.Errorf("%w: %s; give %s", ErrInvalidZone, err.Error(), zoneValuesAccepted)
	}
	return zone, nil
}

// Value returns the setting's zone from the environment, by the rule on
// ZoneSetting. It reads the environment on every call, so a test's
// t.Setenv takes effect at once.
func (s ZoneSetting) Value() Zone {
	raw := os.Getenv(s.Name)
	if raw == "" {
		return s.defaultZone()
	}
	zone, err := s.parse(raw)
	if err != nil {
		fallback := s.defaultZone()
		warnInvalidZone(s, raw, err.Error(), fallback)
		return fallback
	}
	return zone
}

// defaultZone is the zone Default names. Default is written by this
// package in a form parse accepts, so its error is never seen.
func (s ZoneSetting) defaultZone() Zone {
	if s.Default == "" {
		return Zone{}
	}
	zone, _ := s.parse(s.Default)
	return zone
}

// parse reads one non-empty value by the rule on ZoneSetting.
func (s ZoneSetting) parse(raw string) (Zone, error) {
	switch {
	case strings.EqualFold(raw, localZoneName) && s.AcceptsLocal:
		return Zone{Location: time.Local, Name: localZoneName}, nil
	case raw[0] == '+' || raw[0] == '-':
		return parseFixedOffset(raw)
	case strings.EqualFold(raw, localZoneName):
		// time.LoadLocation reads "Local" as the process's zone; only
		// a setting that accepts `local` may name it.
		return Zone{}, fmt.Errorf("the process's zone is not accepted here")
	}
	loc, err := time.LoadLocation(raw)
	if err != nil {
		return Zone{}, fmt.Errorf("not a zone name")
	}
	// time.LoadLocation opens a file named after raw, so a file system
	// that ignores letter case (macOS) finds `utc` and `europe/berlin`
	// too, and the Location keeps raw's spelling. Only the zone
	// database's own spelling is accepted, so every OS accepts the same
	// names, as a browser's list of zones does.
	if !spelledAsInZoneSources(raw, zoneSourceDirs()) {
		return Zone{}, fmt.Errorf("not a zone name as the zone database spells it (letter case counts)")
	}
	return Zone{Location: loc, Name: raw}, nil
}

// utcZoneName is the one zone name time.LoadLocation answers without
// reading a file.
const utcZoneName = "UTC"

// platformZoneDirs are the directories time.LoadLocation looks a zone
// name up in on Unix, in its order (time/zoneinfo_unix.go). Where none
// exists, as on Windows or in a minimal container, it uses the database
// compiled into the binary.
var platformZoneDirs = []string{
	"/usr/share/zoneinfo",
	"/usr/share/lib/zoneinfo",
	"/usr/lib/locale/TZ",
	"/etc/zoneinfo",
}

// zoneSourceDirs is where time.LoadLocation reads zone files from:
// $ZONEINFO first when it is set, then platformZoneDirs. $ZONEINFO may
// name a zip file instead, whose names match exactly; listing it fails,
// which spellingIn reads as a source that does not hold the name.
func zoneSourceDirs() []string {
	if dir := os.Getenv("ZONEINFO"); dir != "" {
		return append([]string{dir}, platformZoneDirs...)
	}
	return platformZoneDirs
}

// spellingInDir is what one zone directory says about a name's
// spelling.
type spellingInDir int

const (
	// nameAbsent: the directory does not hold the name in any case.
	nameAbsent spellingInDir = iota
	// nameExact: every component is an entry of that exact spelling.
	nameExact
	// nameOtherCase: a component is there only in another letter case.
	nameOtherCase
)

// spelledAsInZoneSources reports whether name, a zone name that
// time.LoadLocation accepted, is spelled as the zone database spells it:
// no empty or `.` component, and, in the first of dirs that holds it in
// any letter case, every component an entry of exactly that spelling. A
// name no directory holds came from the database compiled into the
// binary, which matches names exactly, and is accepted.
//
// It reads one directory listing per component and directory, at most
// len(dirs) × 32 listings for a name of 64 bytes, and only directories
// of the zone database. The check runs each time a zone value is parsed,
// a few times per request, never per line.
func spelledAsInZoneSources(name string, dirs []string) bool {
	if name == utcZoneName {
		// time.LoadLocation answers it without a file, so no directory
		// says how it is spelled; and RX_LOG_TZ's default costs no
		// directory listing per request.
		return true
	}
	components := strings.Split(name, "/")
	for _, c := range components {
		if c == "" || c == "." {
			return false
		}
	}
	for _, dir := range dirs {
		switch spellingIn(dir, components) {
		case nameExact:
			return true
		case nameOtherCase:
			return false
		case nameAbsent:
		}
	}
	return true
}

// spellingIn walks components down from dir, one directory listing per
// component, and says whether dir holds them in exactly that spelling,
// only in another letter case, or not at all.
func spellingIn(dir string, components []string) spellingInDir {
	path := dir
	for _, component := range components {
		entries, err := os.ReadDir(path)
		if err != nil {
			return nameAbsent
		}
		switch entrySpelling(entries, component) {
		case nameAbsent:
			return nameAbsent
		case nameOtherCase:
			return nameOtherCase
		case nameExact:
		}
		path = filepath.Join(path, component)
	}
	return nameExact
}

// entrySpelling says whether entries hold name exactly, only in another
// letter case, or not at all.
func entrySpelling(entries []os.DirEntry, name string) spellingInDir {
	found := nameAbsent
	for _, entry := range entries {
		if entry.Name() == name {
			return nameExact
		}
		if strings.EqualFold(entry.Name(), name) {
			found = nameOtherCase
		}
	}
	return found
}

// parseFixedOffset reads `±HH:MM` as a zone that is always that far
// from UTC.
func parseFixedOffset(raw string) (Zone, error) {
	bad := fmt.Errorf("not an offset of the form ±HH:MM up to 18 hours")
	if len(raw) != len("+00:00") || raw[3] != ':' || !twoDigits(raw[1:3]) || !twoDigits(raw[4:6]) {
		return Zone{}, bad
	}
	// Both fields are two digits now, so Atoi cannot fail; checking the
	// digits first matters because Atoi reads a sign of its own, and
	// "+-1:00" would otherwise be an hour west of UTC.
	hours, errH := strconv.Atoi(raw[1:3])
	minutes, errM := strconv.Atoi(raw[4:6])
	if errH != nil || errM != nil || hours > maxFixedOffsetHours || minutes > 59 ||
		(hours == maxFixedOffsetHours && minutes > 0) {
		return Zone{}, bad
	}
	seconds := (hours*60 + minutes) * 60
	if raw[0] == '-' {
		seconds = -seconds
	}
	return Zone{Location: time.FixedZone(raw, seconds), Name: raw}, nil
}

// twoDigits reports whether field is two ASCII digits.
func twoDigits(field string) bool {
	return len(field) == 2 && isDigit(field[0]) && isDigit(field[1])
}

// isDigit reports whether c is an ASCII digit.
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// warnInvalidZone logs one invalid_setting warning for s holding raw,
// unless this process has already logged it.
func warnInvalidZone(s ZoneSetting, raw, problem string, used Zone) {
	if _, loaded := warnedSettings.LoadOrStore(s.Name+"="+raw, struct{}{}); loaded {
		return
	}
	accepted := "UTC, an IANA zone name or ±HH:MM"
	if s.AcceptsLocal {
		accepted += ", or local"
	}
	using := used.Name
	if using == "" {
		using = "(unset)"
	}
	slog.Default().Warn("invalid_setting",
		"name", s.Name,
		"value", raw,
		"problem", problem,
		"accepted", accepted,
		"using", using,
	)
}
