package hooks

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	rxprom "github.com/wlame/rx-go/internal/prometheus"
)

// The hook switches follow the one boolean rule of every RX_* switch:
// the true and false words in any case, and the default, off, for
// anything else.
func TestPolicyFromEnv_OneBooleanRule(t *testing.T) {
	for value, want := range map[string]bool{"true": true, "YES": true, "1": true, "on": true, "false": false, "no": false, "0": false, "off": false, "": false, "maybe": false} {
		t.Setenv("RX_ALLOW_INTERNAL_HOOKS", value)
		t.Setenv("RX_HOOK_STRICT_IP_ONLY", value)
		t.Setenv("RX_DISABLE_CUSTOM_HOOKS", value)
		policy, env := PolicyFromEnv(), HookEnvFromEnv()
		if policy.AllowInternal != want || policy.StrictIPOnly != want || env.DisableCustom != want || env.Policy != policy {
			t.Errorf("%q: policy %+v, disable custom %v; want %v everywhere", value, policy, env.DisableCustom, want)
		}
	}
}

// A validator and a dispatcher built from one snapshot keep it: a later
// change of the environment moves neither, so a loopback URL refused by
// the check is refused by the dialer too.
func TestPolicy_IsASnapshot(t *testing.T) {
	t.Setenv("RX_ALLOW_INTERNAL_HOOKS", "false")
	policy := PolicyFromEnv()
	d := NewDispatcher(DispatcherConfig{Policy: &policy})
	t.Cleanup(func() { d.Close(); d.Wait() })
	t.Setenv("RX_ALLOW_INTERNAL_HOOKS", "true")
	if err := ValidateURL("http://127.0.0.1:9/x", policy); err == nil {
		t.Error("the snapshot accepted a loopback URL after the environment changed")
	}
	if d.cfg.Policy.AllowInternal {
		t.Error("the dispatcher's policy followed the environment")
	}
	if !PolicyFromEnv().AllowInternal {
		t.Error("a new reading does not see the change; the test proves nothing")
	}
}

// The scraped help of the hook metrics is unchanged: a webhook is a
// call, which the help says; the comments above the metrics say GET.
func TestHookMetricsHelpIsUnchanged(t *testing.T) {
	ch := make(chan *prometheus.Desc, 4)
	rxprom.HookCallsTotal.Describe(ch)
	desc := <-ch
	if !strings.Contains(desc.String(), `help: "Total webhook calls, by hook kind and status"`) {
		t.Errorf("rx_hook_calls_total help changed: %s", desc)
	}
}
