package hooks

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wlame/rx-go/internal/trace"
)

// The SSRF guard validates a hook URL once, when it is configured, by
// resolving the hostname and checking the addresses. The HTTP client
// then resolves the same hostname again when the request is made.
//
// Nothing forces the two answers to agree. A name that resolved to a
// public address at configuration time can resolve to 127.0.0.1 by the
// time the request goes out — deliberately, as DNS rebinding, or simply
// because a short-TTL record changed. Validation that happens anywhere
// other than on the address actually dialed is advisory.
//
// These tests drive the dial guard directly, since making a hostname
// change its answer mid-test would need a DNS server.
func TestSafeDialer_RefusesAnInternalAddress(t *testing.T) {
	dial := guardedDialer(nil, false)

	for _, address := range []string{
		"127.0.0.1:80",
		"[::1]:80",
		"10.0.0.1:80",
		"192.168.1.1:80",
		"172.16.0.1:80",
		"169.254.169.254:80", // AWS instance metadata
		"100.64.0.1:80",      // CGNAT
		"0.0.0.0:80",
		"[::ffff:127.0.0.1]:80", // IPv4-mapped loopback
	} {
		conn, err := dial(context.Background(), "tcp", address)
		if err == nil {
			_ = conn.Close()
			t.Errorf("dialing %s was allowed; the SSRF guard must refuse it", address)
			continue
		}
		if !strings.Contains(err.Error(), "refusing to connect") {
			t.Errorf("dialing %s failed with %v, want the guard's refusal", address, err)
		}
	}
}

// The guard must not break the case it exists to protect: a genuinely
// external target still has to be reachable.
func TestSafeDialer_AllowsAPublicAddress(t *testing.T) {
	var dialed atomic.Int32
	inner := func(_ context.Context, _, address string) (net.Conn, error) {
		dialed.Add(1)
		return nil, net.ErrClosed // not connecting for real; only the decision matters
	}
	dial := guardedDialer(inner, false)

	if _, err := dial(context.Background(), "tcp", "93.184.216.34:443"); err != net.ErrClosed {
		t.Fatalf("a public address was refused: %v", err)
	}
	if dialed.Load() != 1 {
		t.Error("the guard did not pass a public address through to the real dialer")
	}
}

// End to end: a dispatcher whose target resolves to loopback must not
// reach a local server, even though the URL passed configuration-time
// validation.
func TestDispatcher_DoesNotReachALoopbackTargetAtRequestTime(t *testing.T) {
	t.Setenv("RX_ALLOW_INTERNAL_HOOKS", "false")

	var hits atomic.Int32
	srv := newLocalServer(t, &hits)

	d := NewDispatcher(DispatcherConfig{
		// Set the URL directly, bypassing ValidateURL, to stand in for a
		// hostname that passed validation and then changed its answer.
		Env:        HookEnv{OnFileURL: srv.URL},
		Workers:    1,
		QueueDepth: 4,
		Timeout:    2 * time.Second,
		Logger:     newSilentLogger(),
	})
	d.OnFile(context.Background(), "/x", trace.FileInfo{})
	d.Close()
	d.Wait()

	if hits.Load() != 0 {
		t.Errorf("the hook reached a loopback server %d times; the dial guard did not fire", hits.Load())
	}
}

func newLocalServer(t *testing.T, hits *atomic.Int32) *localServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusOK)
		}),
		ReadHeaderTimeout: time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return &localServer{URL: "http://" + ln.Addr().String()}
}

type localServer struct{ URL string }

// An operator who opts in with RX_ALLOW_INTERNAL_HOOKS has already said
// that an internal target is intended. The dial guard must respect that
// rather than overriding a documented setting.
func TestSafeDialer_AllowsAnInternalAddressWhenTheOperatorOptedIn(t *testing.T) {
	var dialed atomic.Int32
	inner := func(_ context.Context, _, _ string) (net.Conn, error) {
		dialed.Add(1)
		return nil, net.ErrClosed
	}
	dial := guardedDialer(inner, true)

	if _, err := dial(context.Background(), "tcp", "127.0.0.1:80"); err != net.ErrClosed {
		t.Fatalf("loopback was refused despite the opt-in: %v", err)
	}
	if dialed.Load() != 1 {
		t.Error("the opt-in did not reach the real dialer")
	}
}
