// Package secretsscan implements the `secrets-scan` line detector.
//
// What it does:
//
//   - Flags byte ranges within a line that look like one of five common
//     secret shapes:
//
//     1. AWS access key — the canonical `AKIA` prefix followed by 16
//     uppercase alphanumerics. Used by AWS IAM users for programmatic
//     access and the single highest-signal fingerprint in the set.
//
//     2. GitHub personal access token — `ghp_` prefix followed by 36
//     base62-ish characters. GitHub's classic PAT shape as of 2021+.
//
//     3. Slack token — `xoxb-`, `xoxa-`, `xoxp-`, `xoxr-`, or `xoxs-`
//     followed by dash-separated base62 segments. Covers bot, user,
//     app, refresh, and legacy tokens in one alternative.
//
//     4. JWT-like string — three base64url segments separated by dots,
//     with the first segment starting with the literal `eyJ` (the
//     base64 encoding of `{"`). Not a hard proof of a JWT, but
//     strongly suggests one.
//
//     5. PEM private key header — the literal `-----BEGIN [TYPE ]PRIVATE
//     KEY-----` banner where TYPE is optionally `RSA `, `EC `, `DSA `,
//     or `OPENSSH `.
//
//   - Stateless per-line match — one line at a time, no lookback, no
//     state. Each match emits its own anomaly with a byte range spanning
//     JUST the match (not the whole line). A single line can therefore
//     produce multiple anomalies (e.g. a JWT and a Slack token concatenated
//     in the same log record).
//
//   - Severity 1.0 (loud): per user guidance and plan §Severity
//     Assignments, secrets get the loudest navigation hint. They are
//     almost always worth a human look — false positives are cheap to
//     dismiss but a missed production secret can be very expensive.
//
//   - Category `secrets` so frontends can apply distinct iconography /
//     coloring compared to the `log-traceback` / `log-crash` / `format`
//     buckets.
//
// Regex strategy:
//
//   - One combined regex with five alternations. RE2 (Go's regexp
//     package) is linear-time with no catastrophic backtracking so even
//     five alternations over long lines cost O(line_length). The union
//     is compiled once at package init via MustCompile.
//
//   - FindAllIndex returns every non-overlapping match's byte range
//     within the line; we emit one anomaly per match. Multiple matches
//     on the same line stay separate so UIs can highlight each one
//     individually.
//
// Byte-range semantics on the emitted Anomaly:
//
//   - StartLine == EndLine == ev.Number — single-line match.
//   - StartOffset == ev.StartOffset + matchStart — absolute file offset
//     of the first byte of the secret.
//   - EndOffset == ev.StartOffset + matchEnd — absolute file offset of
//     the byte just PAST the last byte of the secret (end-exclusive,
//     matching LineEvent conventions).
//
// Registration: this package has an init() that calls analyzer.Register
// so a blank import in cmd/rx/main.go is enough to hook it up.
package secretsscan

import (
	"context"
	"regexp"

	"github.com/wlame/rx-go/internal/analyzer"
)

// Metadata constants — kept together at the top so /v1/detectors output
// is trivially auditable against the plan.
const (
	detectorName        = "secrets-scan"
	detectorVersion     = "0.1.0"
	detectorCategory    = "secrets"
	detectorDescription = "Credential-shaped strings (AWS key, GitHub PAT, Slack token, JWT, PEM key)"

	// severity is the plan-mandated value for this detector. Secrets get
	// the loudest navigation hint because false positives are cheap and
	// missed ones are expensive.
	severity = 1.0
)

// combinedSecretRe is one RE2 alternation covering all five secret
// shapes. Compiled once at package init.
//
// Per-alternative notes:
//
//   - AWS access key `AKIA[0-9A-Z]{16}`:
//     The canonical AWS access-key ID shape. 20 total characters, of
//     which the first 4 are the literal `AKIA` and the remaining 16
//     are [0-9A-Z]. No boundary anchoring — matches substring inside
//     longer tokens are a feature (the surrounding characters are
//     usually quotes, equals signs, or JSON delimiters). Mid-token
//     matches in the middle of an unrelated alphanumeric blob can't
//     collide with the AKIA prefix without *being* that prefix.
//
//   - GitHub PAT `ghp_[A-Za-z0-9]{36}`:
//     The classic GitHub personal-access-token shape. The `ghp_`
//     prefix is stable; the 36-char suffix is base62.
//
//   - Slack token `xox[baprs]-[A-Za-z0-9-]+`:
//     Five prefix variants (`xoxb` bot, `xoxa` user, `xoxp` user-legacy,
//     `xoxr` refresh, `xoxs` legacy) followed by `-` then one or more
//     `[A-Za-z0-9-]` characters. The `+` repeat is greedy, matching up
//     to the first non-matching byte (whitespace, quote, equals, etc.).
//
//   - JWT shape `eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`:
//     Three dot-separated base64url segments. The first two are
//     required to start with `eyJ` (base64 of `{"`) which catches all
//     well-formed JWTs (header and payload are both JSON objects).
//     The third segment is the signature, free-form base64url. No
//     attempt to validate structure beyond shape.
//
//   - PEM private key `-----BEGIN (RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----`:
//     The literal banner for a PEM-encoded private key. The
//     non-capturing group `(?:RSA |EC |DSA |OPENSSH )?` makes the
//     algorithm prefix optional, covering both generic PKCS#8
//     (`BEGIN PRIVATE KEY`) and typed banners.
//
// All alternatives are free of backreferences and nested repetition,
// so RE2 can execute the whole union in a single linear pass.
var combinedSecretRe = regexp.MustCompile(
	`AKIA[0-9A-Z]{16}` +
		`|ghp_[A-Za-z0-9]{36}` +
		`|xox[baprs]-[A-Za-z0-9-]+` +
		`|eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+` +
		`|-----BEGIN (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----`,
)

// Detector implements both analyzer.FileAnalyzer and
// analyzer.LineDetector. This detector is stateless per-line: OnLine is
// a pure function of the current line's bytes, with no memory of prior
// lines and no inter-line dependencies. Finalize returns the accumulated
// anomaly list and nothing else.
type Detector struct {
	// out accumulates every emitted anomaly. Appended to in OnLine and
	// returned verbatim from Finalize.
	out []analyzer.Anomaly
}

// New returns a freshly-initialized Detector. Used by tests and by the
// init() registration below.
func New() *Detector {
	return &Detector{}
}

// Name returns the stable registry identifier.
func (d *Detector) Name() string { return detectorName }

// Version returns the semver string for this detector's cache bucket.
func (d *Detector) Version() string { return detectorVersion }

// Category returns the human-readable bucket name for /v1/detectors.
func (d *Detector) Category() string { return detectorCategory }

// Description returns the one-line human summary.
func (d *Detector) Description() string { return detectorDescription }

// Supports says yes to anything. Secret-shaped strings can appear in
// any text-shaped log (stdout/stderr capture, debug dumps, env-var
// snapshots, misconfigured request logs). Non-matching logs simply never
// fire a match so the detector's cost is near-zero on clean data.
func (d *Detector) Supports(_ string, _ string, _ int64) bool {
	return true
}

// Analyze is the FileAnalyzer entry point. The streaming-scan path is
// driven by the coordinator, not Analyze, so this returns an empty
// Report — it exists to satisfy the FileAnalyzer interface for registry
// enumeration.
func (d *Detector) Analyze(_ context.Context, _ analyzer.Input) (*analyzer.Report, error) {
	return &analyzer.Report{
		Name:          detectorName,
		Version:       detectorVersion,
		SchemaVersion: 1,
		Result:        map[string]any{},
	}, nil
}

// OnLine is the streaming-scan hook. Runs the combined regex against
// the current line's bytes and emits one anomaly per non-overlapping
// match.
//
// FindAllIndex returns a slice of [start, end) index pairs; we translate
// each pair into an absolute file-offset range by adding the line's
// StartOffset. The line's LineEvent.Bytes is borrowed storage (see
// LineEvent docs) but we only read it during this call, never retain it.
func (d *Detector) OnLine(w *analyzer.Window) {
	ev := w.Current()
	// FindAllIndex with n=-1 returns every non-overlapping match. On the
	// common case (zero matches) it returns nil with no allocation.
	matches := combinedSecretRe.FindAllIndex(ev.Bytes, -1)
	for _, m := range matches {
		// m is [start, end) within ev.Bytes. Translate to absolute file
		// offsets using the line's StartOffset.
		start := ev.StartOffset + int64(m[0])
		end := ev.StartOffset + int64(m[1])
		d.out = append(d.out, analyzer.Anomaly{
			StartLine:   ev.Number,
			EndLine:     ev.Number,
			StartOffset: start,
			EndOffset:   end,
			Severity:    severity,
			// Category is rewritten to Name() by the coordinator's
			// Finalize (the dedup contract). Keeping the semantic value
			// here helps direct-use code paths (tests that don't go
			// through the coordinator).
			Category:    detectorCategory,
			Description: "credential-shaped string",
		})
	}
}

// Finalize is called once after the last OnLine. Stateless detectors
// have nothing to flush — every match has already been emitted in
// OnLine. The FlushContext argument is unused.
func (d *Detector) Finalize(_ *analyzer.FlushContext) []analyzer.Anomaly {
	return d.out
}

// Compile-time interface conformance checks. If either contract drifts
// we want the build to fail here rather than somewhere deep in wiring.
var (
	_ analyzer.FileAnalyzer = (*Detector)(nil)
	_ analyzer.LineDetector = (*Detector)(nil)
)

// init registers a fresh Detector with the global analyzer registry.
// Callers activate the detector by blank-importing this package in
// cmd/rx/main.go:
//
//	import _ "github.com/wlame/rx-go/internal/analyzer/detectors/secretsscan"
//
// When the builder runs chunk-parallel, each worker must instantiate
// its own Detector (via New) so the accumulated `out` slice doesn't
// leak across workers. The detector is otherwise stateless, so a shared
// instance would produce correct matches but with a race on the slice.
func init() {
	analyzer.Register(New())
}
