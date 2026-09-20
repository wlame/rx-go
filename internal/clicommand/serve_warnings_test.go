package clicommand

import (
	"strings"
	"testing"
)

// rx serve has no authentication by design; these warnings are how a
// configuration that leaves the trusted-network scope gets noticed. A
// bind that other machines can reach and a search root that exposes a
// whole home directory or filesystem each print one warning at startup.
func TestServeWarnings(t *testing.T) {
	const home = "/home/alice"
	cases := []struct {
		name        string
		host        string
		roots       []string
		wantPhrases []string // one per expected warning, in order
	}{
		{"loopback address", "127.0.0.1", []string{"/var/log"}, nil},
		{"loopback name", "localhost", []string{"/var/log"}, nil},
		{"loopback IPv6", "::1", []string{"/var/log"}, nil},
		{"other loopback address", "127.0.0.2", []string{"/var/log"}, nil},
		{"all IPv4 interfaces", "0.0.0.0", []string{"/var/log"}, []string{"0.0.0.0"}},
		{"all IPv6 interfaces", "::", []string{"/var/log"}, []string{"::"}},
		{"empty host is every interface", "", []string{"/var/log"}, []string{"every interface"}},
		{"private address", "10.1.2.3", []string{"/var/log"}, []string{"10.1.2.3"}},
		{"a host name", "logs.internal", []string{"/var/log"}, []string{"logs.internal"}},
		{"home directory root", "127.0.0.1", []string{home}, []string{"home directory"}},
		{"home directory with a trailing slash", "127.0.0.1", []string{home + "/"}, []string{"home directory"}},
		{"filesystem root", "127.0.0.1", []string{"/"}, []string{"filesystem root"}},
		{"a directory inside home", "127.0.0.1", []string{home + "/logs"}, nil},
		{
			"bind and root together",
			"0.0.0.0",
			[]string{"/var/log", "/"},
			[]string{"0.0.0.0", "filesystem root"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := serveWarnings(tc.host, tc.roots, home)

			if len(got) != len(tc.wantPhrases) {
				t.Fatalf("serveWarnings = %q, want %d warnings", got, len(tc.wantPhrases))
			}
			for i, phrase := range tc.wantPhrases {
				if !strings.Contains(got[i], phrase) {
					t.Errorf("warning %d = %q, want it to mention %q", i, got[i], phrase)
				}
			}
		})
	}
}

// The bind warning has to say what to do about it, not only that
// something is wrong.
func TestServeWarnings_BindWarningNamesTheRemedies(t *testing.T) {
	got := serveWarnings("0.0.0.0", []string{"/var/log"}, "/home/alice")

	for _, phrase := range []string{"no authentication", "proxy"} {
		if !strings.Contains(got[0], phrase) {
			t.Errorf("bind warning %q does not mention %q", got[0], phrase)
		}
	}
}

// Without a known home directory only the filesystem root can be judged.
func TestServeWarnings_UnknownHomeIsNotARoot(t *testing.T) {
	if got := serveWarnings("127.0.0.1", []string{"/var/log"}, ""); len(got) != 0 {
		t.Errorf("serveWarnings = %q, want none", got)
	}
}
