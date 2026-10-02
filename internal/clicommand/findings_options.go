package clicommand

import (
	"github.com/wlame/rx-go/internal/findings"
	"github.com/wlame/rx-go/internal/webapi"
)

// findingsScanOptions reads from the environment the settings of a
// signature scan and of the findings cache (webapi.FindingsOptionsFromEnv,
// the one reader `rx serve` uses too): the workers (RX_WORKERS), the
// line view (RX_SIGNATURES_LINE_BYTES), the cap
// (RX_FINDINGS_MAX_PER_SIGNATURE), the timestamp look-back
// (RX_TIMESTAMP_LOOKBACK_KB), the log zone (RX_LOG_TZ) and the cache
// directory. internal/findings and internal/sigscan read no
// environment; every command that scans or keys findings starts from
// these options.
func findingsScanOptions() findings.ScanOptions {
	return webapi.FindingsOptionsFromEnv().Scan
}
