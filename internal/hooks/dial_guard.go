package hooks

import (
	"context"
	"fmt"
	"net"
)

// dialFunc is the shape http.Transport.DialContext expects. Naming it
// lets guardedDialer wrap a real dialer in production and a stub in
// tests.
type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// guardedDialer wraps a dialer so the SSRF policy is applied to the
// address actually being connected to.
//
// ValidateURL checks a hook URL when it is configured: it resolves the
// hostname and rejects internal addresses. The HTTP client then resolves
// that hostname again when the request goes out, and nothing makes the
// two answers agree. A name that resolved to a public address at
// configuration time can resolve to 127.0.0.1 by request time — as a
// deliberate DNS rebinding attack, or simply because a short-TTL record
// changed. A check that does not happen on the address being dialed is
// advisory.
//
// This is the check that is not advisory. By the time DialContext runs,
// resolution has already happened and `address` is a literal IP and
// port, so there is no window left for the answer to change.
//
// allowInternal is the same opt-in ValidateURL honors,
// RX_ALLOW_INTERNAL_HOOKS. An operator who deliberately points a hook at
// a collector on localhost has said so once; the dial guard must not
// silently override that decision.
//
// inner is the dialer to delegate to once the address is allowed; nil
// means the default net.Dialer.
func guardedDialer(inner dialFunc, allowInternal bool) dialFunc {
	if allowInternal {
		if inner == nil {
			return (&net.Dialer{}).DialContext
		}
		return inner
	}
	if inner == nil {
		inner = (&net.Dialer{}).DialContext
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("refusing to connect to %q: %w", address, err)
		}
		ip := net.ParseIP(host)
		if ip == nil {
			// DialContext is called after resolution, so a non-literal
			// host here means something unexpected. Refuse rather than
			// guess.
			return nil, fmt.Errorf("refusing to connect to %q: not a resolved address", address)
		}
		if reason := internalIPReason(ip); reason != "" {
			return nil, fmt.Errorf("refusing to connect to %s: it %s", address, reason)
		}
		return inner(ctx, network, address)
	}
}
