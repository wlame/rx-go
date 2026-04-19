// Package clicommand implements the cobra commands that make up the rx
// binary's CLI surface.
//
// There is one file per subcommand (trace, samples, index, compress,
// serve) plus common.go for shared flag plumbing. The cmd/rx/main.go
// entry point assembles these into the root command and handles the
// "default subcommand is trace" dispatch rule.
//
// # Mapping from Python CLI
//
// The Python version uses click; we use cobra. The flag surface matches
// Python's exactly so user muscle memory carries over:
//
//	rx "pattern" /var/log/app.log          — trace (default subcommand)
//	rx trace ... --max-results=N          — explicit trace
//	rx samples /var/log/app.log --lines=100
//	rx index /var/log/app.log --analyze
//	rx compress /var/log/app.log --frame-size=4M
//	rx serve --host=0.0.0.0 --port=8000
//
// # Global flags
//
//	--no-color        Disable ANSI color (also NO_COLOR / RX_NO_COLOR env)
//	--json            JSON output (per subcommand, not global)
//	--debug           Emit .debug_* files (per subcommand, not global)
//	--help / -h       Show help
//	--version         Show version
package clicommand
