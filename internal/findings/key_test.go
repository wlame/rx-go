package findings

import (
	"regexp"
	"strings"
	"testing"

	"github.com/wlame/rx-go/internal/sigscan"
)

// The probes of the key tests, one per kind; the variants below edit
// them.
const (
	probeRegex = `
  - id: probe
    name: Probe
    group: severity
    description: A probe.
    regex: 'ERROR (\w+)'
`
	probeBlock = `
  - id: probe
    name: Probe
    group: traceback
    description: A probe.
    block:
      start: '^Traceback'
      continue: '^\s'
      max_lines: 100
`
	probeRun = `
  - id: probe
    name: Probe
    group: structure
    description: A probe.
    run: {match: 'retry', min: 3}
`
	probeMeasure = `
  - id: probe
    name: Probe
    group: structure
    description: A probe.
    measure: {name: long-line}
`
	otherRedactor = `
  - id: other-key
    name: Other key
    group: secret
    description: Another invented key.
    redact: true
    regex: 'TOKEN-[a-z]{8}'
`
	plainOther = `
  - id: plain-other
    name: Plain other
    group: severity
    description: A signature that does not redact.
    regex: 'FATAL'
`
)

// keyFor loads defs and returns the key of the signature "probe".
func keyFor(t *testing.T, opts ScanOptions, defs ...string) string {
	t.Helper()
	cat := loadCatalogue(t, defs...)
	return Key(pick(t, cat, "probe")[0], cat, opts)
}

// add appends YAML lines to a signature definition, indented as its
// fields.
func add(def string, lines ...string) string {
	return def + "    " + strings.Join(lines, "\n    ") + "\n"
}

// Every part of a signature that can change its findings, and every
// setting that changes an answer, changes the key; the descriptive
// fields, and the settings that change only how a scan runs, do not.
func TestKey_ChangesExactlyWithWhatChangesAnAnswer(t *testing.T) {
	base := testOptions(t)
	cases := []struct {
		name          string
		before, after []string
		opts          func(*ScanOptions)
		same          bool
	}{
		{name: "regex", before: []string{probeRegex}, after: []string{strings.Replace(probeRegex, `ERROR`, `FATAL`, 1)}},
		{name: "not", before: []string{probeRegex}, after: []string{add(probeRegex, "not: 'debug'")}},
		{name: "capture", before: []string{probeRegex}, after: []string{add(probeRegex, "capture: 1")}},
		{name: "entropy", before: []string{probeRegex}, after: []string{add(probeRegex, "entropy: 2.5")}},
		{name: "validate", before: []string{probeRegex}, after: []string{add(probeRegex, "validate: ipv4")}},
		{name: "keywords", before: []string{probeRegex}, after: []string{add(probeRegex, "keywords: [error]")}},
		{name: "redact", before: []string{probeRegex}, after: []string{add(probeRegex, "redact: true")}},
		{name: "kind", before: []string{probeRegex}, after: []string{strings.Replace(probeRun, "group: structure", "group: severity", 1)}},
		{name: "block max_lines", before: []string{probeBlock}, after: []string{strings.Replace(probeBlock, "max_lines: 100", "max_lines: 50", 1)}},
		{name: "block min_lines", before: []string{probeBlock}, after: []string{add(probeBlock, "  min_lines: 2")}},
		{name: "block continue", before: []string{probeBlock}, after: []string{strings.Replace(probeBlock, `'^\s'`, `'^\s+at '`, 1)}},
		{name: "block end", before: []string{probeBlock}, after: []string{add(probeBlock, "  end: '^\\w+Error'")}},
		{name: "run min", before: []string{probeRun}, after: []string{strings.Replace(probeRun, "min: 3", "min: 4", 1)}},
		{name: "run not", before: []string{probeRun}, after: []string{strings.Replace(probeRun, "min: 3", "min: 3, not: 'ok'", 1)}},
		{name: "measure parameter", before: []string{probeMeasure}, after: []string{strings.Replace(probeMeasure, "long-line", "long-line, factor: 20", 1)}},
		{name: "measure name", before: []string{probeMeasure}, after: []string{strings.Replace(probeMeasure, "long-line", "binary", 1)}},
		{name: "line view", before: []string{probeRegex}, opts: func(o *ScanOptions) { o.Scan.LineBytes = 2048 }},
		{name: "cap", before: []string{probeRegex}, opts: func(o *ScanOptions) { o.Scan.Cap = 500 }},
		{name: "lookback", before: []string{probeRegex}, opts: func(o *ScanOptions) { o.Scan.LookbackBytes = 0 }},
		{name: "log zone", before: []string{probeRegex}, opts: func(o *ScanOptions) { o.LogZone = "Europe/Berlin" }},
		{name: "a redactor added", before: []string{probeRegex}, after: []string{probeRegex, otherRedactor}},
		{name: "a redactor changed", before: []string{probeRegex, otherRedactor}, after: []string{probeRegex, strings.Replace(otherRedactor, "{8}", "{9}", 1)}},
		{name: "a redactor's id", before: []string{probeRegex, otherRedactor}, after: []string{probeRegex, strings.Replace(otherRedactor, "other-key", "second-key", 1)}},

		{name: "name", before: []string{probeRegex}, after: []string{strings.Replace(probeRegex, "name: Probe", "name: Renamed", 1)}, same: true},
		{name: "description", before: []string{probeRegex}, after: []string{strings.Replace(probeRegex, "A probe.", "Another text.", 1)}, same: true},
		{name: "group", before: []string{probeRegex}, after: []string{strings.Replace(probeRegex, "group: severity", "group: system", 1)}, same: true},
		{name: "descriptive fields", before: []string{probeRegex}, after: []string{add(probeRegex, "tags: [web]", "level: high",
			"links: [https://example.com/probe]", "metadata: {team: core}", "default: false",
			"examples: {match: ['ERROR boom'], no_match: ['INFO fine']}")}, same: true},
		{name: "workers", before: []string{probeRegex}, opts: func(o *ScanOptions) { o.Scan.Workers = 7 }, same: true},
		{name: "run flags", before: []string{probeRegex}, opts: func(o *ScanOptions) {
			o.Force, o.NoCache, o.Spooled, o.Dir = true, true, true, "/elsewhere"
			o.Progress = func(int64, int64) {}
		}, same: true},
		{name: "default line view and cap named", before: []string{probeRegex}, opts: func(o *ScanOptions) {
			o.Scan.LineBytes, o.Scan.Cap = sigscan.DefaultLineBytes, sigscan.DefaultCap
		}, same: true},
		{name: "redactors in another order", before: []string{probeRegex, defKey, otherRedactor}, after: []string{probeRegex, otherRedactor, defKey}, same: true},
		{name: "another signature that does not redact", before: []string{probeRegex, plainOther}, after: []string{probeRegex, strings.Replace(plainOther, "FATAL", "PANIC", 1)}, same: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			after := tc.after
			if after == nil {
				after = tc.before
			}
			opts := base
			if tc.opts != nil {
				tc.opts(&opts)
			}
			k1, k2 := keyFor(t, base, tc.before...), keyFor(t, opts, after...)
			if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(k1) {
				t.Fatalf("key %q is not a hex sha256", k1)
			}
			if (k1 == k2) != tc.same {
				t.Errorf("same key: %v, want %v", k1 == k2, tc.same)
			}
		})
	}
}

// The engine version enters every key; a measure's version enters the
// keys of its signatures only.
func TestKey_EngineAndMeasureVersions(t *testing.T) {
	opts := testOptions(t)
	regexCat := loadCatalogue(t, probeRegex)
	measureCat := loadCatalogue(t, probeMeasure)
	regexSig, measureSig := pick(t, regexCat, "probe")[0], pick(t, measureCat, "probe")[0]
	bumped := func(name string) int { return sigscan.MeasureVersion(name) + 1 }

	if scopeFor(regexCat, opts, sigscan.EngineVersion+1, sigscan.MeasureVersion).key(regexSig) == Key(regexSig, regexCat, opts) {
		t.Error("a new engine version kept the key")
	}
	if scopeFor(regexCat, opts, sigscan.EngineVersion, bumped).key(regexSig) != Key(regexSig, regexCat, opts) {
		t.Error("a measure version changed the key of a regex signature")
	}
	if scopeFor(measureCat, opts, sigscan.EngineVersion, bumped).key(measureSig) == Key(measureSig, measureCat, opts) {
		t.Error("a new measure version kept the key of its signature")
	}
}
