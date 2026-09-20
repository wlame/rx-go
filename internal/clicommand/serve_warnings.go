package clicommand

import (
	"fmt"
	"net"
	"path/filepath"
)

// serveWarnings returns the warnings `rx serve` prints at startup for a
// configuration that reaches past its intended scope: a trusted
// internal network, with the operator providing the perimeter.
//
// It warns rather than refuses. A bind other machines can reach is a
// legitimate setup behind a VPN or a proxy, and refusing would break
// every deployment that already relies on it; what matters is that the
// choice is visible in the log of whoever starts the server.
//
// host is the --host value; roots are the resolved search roots; home
// is the user's home directory, or "" when it is not known; tokenSet
// says whether RX_API_TOKEN guards the /v1 API.
func serveWarnings(host string, roots []string, home string, tokenSet bool) []string {
	var warnings []string
	if w := bindWarning(host, tokenSet); w != "" {
		warnings = append(warnings, w)
	}
	for _, root := range roots {
		if w := searchRootWarning(root, home); w != "" {
			warnings = append(warnings, w)
		}
	}
	return warnings
}

// isLoopbackHost reports whether host names only this machine: the
// name "localhost" or any loopback address (127.0.0.0/8, ::1).
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// bindWarning explains a bind other machines can reach, or returns "".
// An empty host is Go's spelling of "every interface".
//
// Without a token the API is open to anyone who reaches the port. With
// one, the API is guarded but the token crosses plain HTTP in clear text,
// which is what the warning then says.
func bindWarning(host string, tokenSet bool) string {
	if isLoopbackHost(host) {
		return ""
	}
	where := host
	if host == "" {
		where = "every interface"
	}
	if tokenSet {
		return fmt.Sprintf("Warning: listening on %s, which other machines can reach. "+
			"/v1 requires the API token, but it crosses plain HTTP in clear text: "+
			"use a VPN, an SSH tunnel or a TLS proxy for anything beyond a trusted network.",
			where)
	}
	return fmt.Sprintf("Warning: listening on %s, which other machines can reach. "+
		"rx serve has no authentication: set RX_API_TOKEN to require a token, "+
		"or put an authenticating proxy in front. rx is meant for a trusted internal network.",
		where)
}

// searchRootWarning explains a search root that serves far more than a
// log directory — the filesystem root or the home directory — or
// returns "". Hidden entries stay refused either way, which the warning
// says so the operator knows what is and is not exposed.
func searchRootWarning(root, home string) string {
	clean := filepath.Clean(root)
	switch {
	case clean == string(filepath.Separator):
		return fmt.Sprintf("Warning: the search root %s is the filesystem root; "+
			"every file the server can read, except hidden ones, is served.", clean)
	case home != "" && clean == filepath.Clean(home):
		return fmt.Sprintf("Warning: the search root %s is your home directory; "+
			"every file in it, except hidden ones, is served.", clean)
	}
	return ""
}
