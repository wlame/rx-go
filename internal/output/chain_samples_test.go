package output

import (
	"strings"
	"testing"
	"time"

	"github.com/wlame/rx-go/pkg/rxtypes"
)

// Before the chain is ready a part is read alone: its lines carry `?`
// for their global number, the head of a key names the part, and no
// time span is shown.
func TestFormatChainSamples_APendingPart(t *testing.T) {
	three := int64(3)
	resp := &rxtypes.ChainSamplesResponse{
		Path: "/var/log/app.log", State: rxtypes.ChainStatePending, Fingerprint: "0123456789abcdef",
		Parts:         []rxtypes.ChainPart{{Name: "app.log.1", LineCount: &three}, {Name: "app.log"}},
		BeforeContext: 1, AfterContext: 0,
		Lines: map[string]int64{"2": -1},
		Samples: map[string][]rxtypes.ChainPiece{"2": {{
			Part: "app.log.1", FirstLocalLine: 1, FirstGlobalLine: -1, Lines: []string{"one", "two"}, PartStart: true,
		}}},
	}
	got := FormatChainSamples(resp, "app.log.1", NewChainTimes(resp.Parts, time.UTC), "UTC")
	want := strings.Join([]string{
		"Chain: /var/log/app.log  pending  2 parts  fingerprint 0123456789abcdef",
		"Context: 1 before, 0 after",
		"",
		"=== /var/log/app.log app.log.1:2 ===",
		"-- app.log.1 --",
		"?  app.log.1:1  one",
		"?  app.log.1:2  two",
		"",
	}, "\n")
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}
