package findings

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"

	"github.com/wlame/rx-go/internal/signatures"
	"github.com/wlame/rx-go/internal/sigscan"
)

// keyFormat names the layout of the bytes a key hashes. Changing what
// goes into a key changes this text, so every old key stops matching.
const keyFormat = "rx-findings-key/1"

// Key returns the key of a signature's cache entry, as hex sha256: what
// an entry must carry to be current for sig loaded in cat and scanned
// with opts.
//
// The key hashes the signature's matching part after defaults (kind,
// anchor and every regex, keyword clauses, not, capture, entropy,
// validate, the block, run or measure parameters, redact), the engine
// version, the measure's version, the settings that change answers
// (opts.Scan.LineBytes, Cap and LookbackBytes, opts.LogZone) and a
// hash of the matching parts of every redacting signature of cat
// (cat.Redactors(), by id, each with its id), since every excerpt is
// masked by all of them. Name, description, group, tags, level, links,
// metadata, examples and `default` are not in it, nor the workers or
// any other setting that changes only how a scan runs.
//
// Zero LineBytes and Cap mean the scan's defaults, and a negative
// LookbackBytes means 0, as in the scan, so both spellings give one key.
func Key(sig *signatures.Signature, cat *signatures.Catalogue, opts ScanOptions) string {
	return newKeyScope(cat, opts).key(sig)
}

// keyScope is what every key of one call shares: the settings and the
// redacting set, as canonical bytes, and the measure versions; and the
// key of the masked ranges file.
type keyScope struct {
	// shared is the canonical JSON of keySettings.
	shared []byte
	// rangesKey is the key of the file's masked ranges: the hex sha256 of
	// the engine version, the line view and the redacting set, which
	// decide where the bodies of redacting blocks and runs lie (the
	// view decides whether a line opens or continues a block).
	rangesKey string
	// measureVersion is sigscan.MeasureVersion (tests give another).
	measureVersion func(name string) int
}

// newKeyScope is the scope of cat and opts with this build's engine and
// measure versions.
func newKeyScope(cat *signatures.Catalogue, opts ScanOptions) keyScope {
	return scopeFor(cat, opts, sigscan.EngineVersion, sigscan.MeasureVersion)
}

// keySettings is the part of every key that does not depend on the
// signature.
type keySettings struct {
	Format        string `json:"format"`
	Engine        int    `json:"engine"`
	LineBytes     int    `json:"line_bytes"`
	Cap           int    `json:"cap"`
	LookbackBytes int    `json:"lookback_bytes"`
	LogZone       string `json:"log_zone"`
	// Redactors is the hex sha256 of the redacting set (redactorsHash).
	Redactors string `json:"redactors"`
}

// scopeFor builds a scope with the engine and measure versions given.
func scopeFor(cat *signatures.Catalogue, opts ScanOptions, engine int, measureVersion func(string) int) keyScope {
	settings := keySettings{
		Format:        keyFormat,
		Engine:        engine,
		LineBytes:     opts.Scan.LineBytes,
		Cap:           opts.Scan.Cap,
		LookbackBytes: max(opts.Scan.LookbackBytes, 0),
		LogZone:       opts.LogZone,
		Redactors:     redactorsHash(cat.Redactors()),
	}
	// The scan reads zero (or less) as its defaults; the key does too.
	if settings.LineBytes <= 0 {
		settings.LineBytes = sigscan.DefaultLineBytes
	}
	if settings.Cap <= 0 {
		settings.Cap = sigscan.DefaultCap
	}
	ranges := rangesSettings{Format: rangesKeyFormat, Engine: engine, LineBytes: settings.LineBytes, Redactors: settings.Redactors}
	sum := sha256.Sum256(canonicalJSON(ranges))
	return keyScope{shared: canonicalJSON(settings), rangesKey: hex.EncodeToString(sum[:]), measureVersion: measureVersion}
}

// rangesKeyFormat names the layout of the bytes a ranges key hashes.
const rangesKeyFormat = "rx-findings-ranges/1"

// rangesSettings is what the key of a file's masked ranges hashes.
type rangesSettings struct {
	Format    string `json:"format"`
	Engine    int    `json:"engine"`
	LineBytes int    `json:"line_bytes"`
	// Redactors is the hex sha256 of the redacting set (redactorsHash).
	Redactors string `json:"redactors"`
}

// key is the hex sha256 of the shared bytes and sig's matching part,
// with its measure's version.
func (k keyScope) key(sig *signatures.Signature) string {
	part := matchingPartOf(sig)
	if part.Measure != nil {
		part.Measure.Version = k.measureVersion(part.Measure.Name)
	}
	h := sha256.New()
	// Go note: a hash.Hash's Write never returns an error.
	h.Write(k.shared)
	h.Write([]byte{0})
	h.Write(canonicalJSON(part))
	return hex.EncodeToString(h.Sum(nil))
}

// redactorsHash is the hex sha256 of the redacting signatures' ids and
// matching parts, sorted by id: the catalogue's order (group order,
// then load order) is not part of what masks a value, so moving a
// group does not make every entry stale.
func redactorsHash(redactors []*signatures.Signature) string {
	type named struct {
		ID   string          `json:"id"`
		Part json.RawMessage `json:"part"`
	}
	list := make([]named, 0, len(redactors))
	for _, r := range redactors {
		list = append(list, named{ID: r.ID, Part: canonicalJSON(matchingPartOf(r))})
	}
	slices.SortFunc(list, func(a, b named) int { return cmp.Compare(a.ID, b.ID) })
	sum := sha256.Sum256(canonicalJSON(list))
	return hex.EncodeToString(sum[:])
}

// matchingPart is the part of a signature that decides its findings,
// in a fixed shape: encoding/json writes struct fields in declaration
// order, every regex as its source text, and a nil pointer as null, so
// equal definitions give equal bytes.
type matchingPart struct {
	Kind signatures.Kind `json:"kind"`
	// Keywords are the keyword clauses of the gate (nil: no gate).
	Keywords [][]string   `json:"keywords"`
	Redact   bool         `json:"redact"`
	Regex    *regexPart   `json:"regex"`
	Block    *blockPart   `json:"block"`
	Run      *runPart     `json:"run"`
	Measure  *measurePart `json:"measure"`
}

type regexPart struct {
	Regex    string   `json:"regex"`
	Not      *string  `json:"not"`
	Capture  int      `json:"capture"`
	Entropy  *float64 `json:"entropy"`
	Validate string   `json:"validate"`
}

type statePart struct {
	State string `json:"state"`
	Regex string `json:"regex"`
	Next  string `json:"next"`
	End   bool   `json:"end"`
}

type blockPart struct {
	States   []statePart `json:"states"`
	Start    *string     `json:"start"`
	Lines    int         `json:"lines"`
	MinLines int         `json:"min_lines"`
	MaxLines int         `json:"max_lines"`
	Not      *string     `json:"not"`
}

type runPart struct {
	Match *string `json:"match"`
	Not   *string `json:"not"`
	Blank bool    `json:"blank"`
	Same  bool    `json:"same"`
	Min   int     `json:"min"`
}

type measurePart struct {
	Name string `json:"name"`
	// Params is MeasureParams.Canonical: every parameter after defaults.
	Params  string `json:"params"`
	Version int    `json:"version"`
}

// matchingPartOf reads sig's matching part. The measure's version is
// left 0; keyScope.key fills it.
func matchingPartOf(sig *signatures.Signature) matchingPart {
	part := matchingPart{Kind: sig.Kind, Keywords: sig.Clauses, Redact: sig.Redact}
	switch {
	case sig.Regex != nil:
		r := sig.Regex
		part.Regex = &regexPart{Regex: r.Regex.String(), Not: source(r.Not), Capture: r.Capture, Entropy: r.Entropy, Validate: r.Validate}
	case sig.Block != nil:
		b := sig.Block
		bp := &blockPart{Start: source(b.Start), Lines: b.Lines, MinLines: b.MinLines, MaxLines: b.MaxLines, Not: source(b.Not)}
		for _, rule := range b.States {
			bp.States = append(bp.States, statePart{State: rule.State, Regex: deref(source(rule.Regex)), Next: rule.Next, End: rule.End})
		}
		part.Block = bp
	case sig.Run != nil:
		r := sig.Run
		part.Run = &runPart{Match: source(r.Match), Not: source(r.Not), Blank: r.Blank, Same: r.Same, Min: r.Min}
	case sig.Measure != nil:
		part.Measure = &measurePart{Name: sig.Measure.Name, Params: string(sig.Measure.Params.Canonical())}
	}
	return part
}

// source is a regex's source text, nil for no regex.
func source(re *regexp.Regexp) *string {
	if re == nil {
		return nil
	}
	s := re.String()
	return &s
}

// deref is the string p points at, "" for nil.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// canonicalJSON encodes v, a value of the fixed shapes above. They hold
// only strings, numbers (finite: the loader refuses NaN), booleans,
// slices and pointers to such, which encoding/json always encodes, so
// an error here is a programming error.
func canonicalJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("findings: encode a key part: %v", err))
	}
	return data
}
