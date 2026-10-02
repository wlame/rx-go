package sigscan

// Tests of the built-in signature catalogue as a whole: what it holds,
// that every built-in carries examples the engine confirms, and the
// rules the groups keep (secrets redact, personal data is opt-in).

import (
	"slices"
	"testing"

	"github.com/wlame/rx-go/internal/signatures"
)

// builtinCatalogue loads the built-in signatures alone, with their
// examples run by this engine.
func builtinCatalogue(t testing.TB) *signatures.Catalogue {
	t.Helper()
	cat := signatures.Load(signatures.LoadOptions{Paths: []string{}, CheckExamples: CheckExamples})
	if errs := cat.Errors(); len(errs) > 0 {
		t.Fatalf("the built-in catalogue does not load cleanly:\n%v", errs)
	}
	return cat
}

// builtinGroups are the built-in groups in their order, each with the
// ids of its signatures in catalogue order.
var builtinGroups = []struct {
	name string
	ids  []string
}{
	{"traceback", []string{"python-traceback", "java-exception", "dotnet-exception", "js-error", "go-panic", "ruby-backtrace", "php-fatal", "rust-panic"}},
	{"crash", []string{"sanitizer-report", "glibc-abort", "killed-by-signal", "kernel-segfault", "systemd-coredump", "kernel-oops", "oom-killer", "k8s-crash-loop", "jvm-fatal-error"}},
	{"secret", []string{"aws-access-key", "aws-secret-key", "github-token", "gitlab-token", "slack-token", "slack-webhook", "stripe-key", "google-api-key",
		"anthropic-key", "openai-key", "jwt", "private-key", "password-assignment", "url-password", "authorization-header", "high-entropy-token"}},
	{"pii", []string{"email-address", "ipv4-address", "ipv6-address", "card-number", "iban"}},
	{"sql", []string{"sql-statement", "dangerous-sql", "postgres-explain", "postgres-slow-statement", "postgres-deadlock", "postgres-lock-wait", "mysql-slow-query"}},
	{"http", []string{"http-5xx"}},
	{"severity", []string{"error-line", "warning-line"}},
	{"system", []string{"disk-full", "too-many-open-files", "out-of-memory", "connection-failure", "network-timeout", "tls-failure", "dns-failure"}},
	{"lifecycle", []string{"signal-received", "exit-status", "systemd-unit-change"}},
	{"structure", []string{"blank-run", "long-line", "repeated-lines", "repeat-suppressed", "json-block", "binary-bytes", "base64-blob", "hex-dump", "unprefixed-run"}},
	{"time", []string{"log-burst", "time-gap", "time-backwards"}},
}

// optInBuiltins are the built-ins with `default: false`: every one of
// the pii group, and warning-line.
var optInBuiltins = []string{"email-address", "ipv4-address", "ipv6-address", "card-number", "iban", "warning-line"}

// The embedded catalogue loads with zero errors and holds 70
// signatures in 11 groups, in the groups' order and with the ids the
// catalogue lists, each in its own group.
func TestBuiltinCatalogue_SeventySignaturesInElevenGroups(t *testing.T) {
	cat := builtinCatalogue(t)
	var wantIDs, wantGroups []string
	for _, g := range builtinGroups {
		wantGroups = append(wantGroups, g.name)
		wantIDs = append(wantIDs, g.ids...)
	}
	var gotGroups []string
	for _, g := range cat.Groups() {
		gotGroups = append(gotGroups, g.Name)
	}
	if !slices.Equal(gotGroups, wantGroups) {
		t.Errorf("groups %v, want %v", gotGroups, wantGroups)
	}
	var gotIDs []string
	for _, s := range cat.Signatures() {
		gotIDs = append(gotIDs, s.ID)
	}
	if len(wantIDs) != 70 || len(wantGroups) != 11 {
		t.Fatalf("the expectation lists %d ids in %d groups", len(wantIDs), len(wantGroups))
	}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("ids %v,\nwant %v", gotIDs, wantIDs)
	}
	for _, g := range builtinGroups {
		for _, id := range g.ids {
			if s, _ := cat.Lookup(id); s.Group != g.name || !s.Source.Builtin {
				t.Errorf("%s: group %q, built in %v; want group %q", id, s.Group, s.Source.Builtin, g.name)
			}
		}
	}
}

// Every built-in has at least one `match` and one `no_match` example,
// and the engine confirms all of them (the load above ran them; this
// checks they exist and runs them once more by hand).
func TestBuiltinCatalogue_EveryBuiltinHasPassingExamples(t *testing.T) {
	for _, s := range builtinCatalogue(t).Signatures() {
		if len(s.Examples.Match) == 0 || len(s.Examples.NoMatch) == 0 {
			t.Errorf("%s: %d match and %d no_match examples, want at least one of each", s.ID, len(s.Examples.Match), len(s.Examples.NoMatch))
			continue
		}
		if err := CheckExamples(s); err != nil {
			t.Errorf("%s: %v", s.ID, err)
		}
	}
}

// Every secret signature redacts; every pii signature and warning-line
// are opt-in, and every other built-in is in the default set. Each
// built-in has a description and at least one link.
func TestBuiltinCatalogue_SecretsRedactAndOptInSignatures(t *testing.T) {
	cat := builtinCatalogue(t)
	for _, s := range cat.Signatures() {
		if s.Group == "secret" && !s.Redact {
			t.Errorf("%s: a secret signature without redact: true", s.ID)
		}
		if wantDefault := !slices.Contains(optInBuiltins, s.ID); s.Default != wantDefault {
			t.Errorf("%s: default %v, want %v", s.ID, s.Default, wantDefault)
		}
		if s.Description == "" || len(s.Links) == 0 {
			t.Errorf("%s: description %q, %d links; want both", s.ID, s.Description, len(s.Links))
		}
	}
	if got := len(cat.DefaultSet()); got != 70-len(optInBuiltins) {
		t.Errorf("the default set holds %d signatures, want %d", got, 70-len(optInBuiltins))
	}
}
