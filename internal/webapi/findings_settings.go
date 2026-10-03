package webapi

import (
	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/findings"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/internal/sigscan"
)

// FindingsOptionsFromEnv reads from the environment the settings of a
// signature scan, of the findings cache and of the text of findings:
// the workers (RX_WORKERS), the line view (RX_SIGNATURES_LINE_BYTES),
// the cap (RX_FINDINGS_MAX_PER_SIGNATURE), the timestamp look-back
// (RX_TIMESTAMP_LOOKBACK_KB), the log zone (RX_LOG_TZ), the cache
// directory, the line text limit (RX_MAX_LINE_TEXT_BYTES), the text
// budget (RX_FINDINGS_TEXT_MAX_BYTES), whether line indexes are read
// (RX_NO_INDEX), and whether secrets are masked (RX_MASK_SECRETS; the
// --mask-secrets flag of `rx trace` and `rx serve` overrides it).
//
// internal/findings and internal/sigscan read no environment: `rx
// serve` calls this once when it starts and hands the result to the
// server (Config.Findings), and the commands that scan or key findings
// start from it too.
func FindingsOptionsFromEnv() findings.TraceOptions {
	zone := config.LogTZ()
	loader := samples.IndexLoader(samples.StoredIndex)
	if config.GetBoolEnv("RX_NO_INDEX", false) {
		loader = samples.NoIndex
	}
	return findings.TraceOptions{
		Scan: findings.ScanOptions{
			Scan: sigscan.Options{
				Workers:       config.Workers(),
				LineBytes:     config.SignaturesLineBytes(),
				Cap:           config.FindingsMaxPerSignature(),
				LookbackBytes: int(config.TimestampLookbackBytes()),
			},
			LogZone:     zone.Name,
			Dir:         config.GetFindingsCacheDir(),
			MaskSecrets: config.MaskSecrets(),
		},
		LineTextBytes: config.MaxLineTextBytes(),
		TextBudget:    config.FindingsTextMaxBytes(),
		LogZone:       zone.Location,
		IndexLoader:   loader,
	}
}
