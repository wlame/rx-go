package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
)

// IntSetting describes one integer environment variable: its name, the
// value rx uses when the variable is unset or not acceptable, and the
// range of values it accepts.
//
// Every integer variable rx reads follows one rule, applied by Value:
//
//   - unset or empty: Default, silently;
//   - not a whole decimal number, or below Min: Default, with a warning;
//   - above Max: Max, with a warning.
//
// The warning is one "invalid_setting" log line per variable and value
// in a process, naming the variable, the value it holds, the accepted
// range and the value used instead.
type IntSetting struct {
	// Name is the environment variable, e.g. "RX_LARGE_FILE_MB".
	Name string
	// Default is the value used when Name is unset or holds a value
	// below Min or not a number.
	Default int
	// Min and Max bound the values Name accepts, both included.
	Min int
	Max int
}

// Bounds shared by several settings.
const (
	// maxSizeMB is the largest size setting in MB: 1 TiB. A size in MB
	// is turned into bytes as int64(v) << 20, which this keeps far
	// below the int64 limit, and no file rx is meant for comes near it.
	maxSizeMB = 1 << 20

	// maxConcurrency caps how many ripgrep processes run at once and
	// how many chunks one file is split into. Each worker is a ripgrep
	// process with its own pipes and buffers, so the cap bounds the
	// processes, file descriptors and memory one search can take,
	// whatever the environment asks for.
	maxConcurrency = 256
)

// The integer settings, as data. docs/configuration.md lists the same
// rows ("Integer settings"), and a test keeps the two in step.
var (
	// WorkersSetting is RX_WORKERS: how many ripgrep processes a search
	// runs at once. Default 0 means "not set": the worker count is then
	// the smaller of the CPU count and RX_MAX_SUBPROCESSES.
	WorkersSetting = IntSetting{Name: "RX_WORKERS", Default: 0, Min: 1, Max: maxConcurrency}

	// MaxSubprocessesSetting is RX_MAX_SUBPROCESSES.
	MaxSubprocessesSetting = IntSetting{Name: "RX_MAX_SUBPROCESSES", Default: DefaultMaxSubprocesses, Min: 1, Max: maxConcurrency}

	// MinChunkSizeMBSetting is RX_MIN_CHUNK_SIZE_MB.
	MinChunkSizeMBSetting = IntSetting{Name: "RX_MIN_CHUNK_SIZE_MB", Default: DefaultMinChunkSizeMB, Min: 1, Max: maxSizeMB}

	// LargeFileMBSetting is RX_LARGE_FILE_MB. Its minimum keeps the
	// index checkpoint step (a fiftieth of it) above 20 KB: at 0 or
	// below, every line became a checkpoint and every plain file
	// counted as large.
	LargeFileMBSetting = IntSetting{Name: "RX_LARGE_FILE_MB", Default: DefaultLargeFileMB, Min: 1, Max: maxSizeMB}

	// MaxLineTextBytesSetting is RX_MAX_LINE_TEXT_BYTES. Its maximum,
	// 256 MiB, bounds what one line can hold in memory while ripgrep's
	// output is read.
	MaxLineTextBytesSetting = IntSetting{Name: "RX_MAX_LINE_TEXT_BYTES", Default: DefaultMaxLineTextBytes, Min: 1, Max: 256 << 20}

	// MaxSubmatchesPerLineSetting is RX_MAX_SUBMATCHES_PER_LINE. Its
	// maximum bounds the submatch records one line can hold.
	MaxSubmatchesPerLineSetting = IntSetting{Name: "RX_MAX_SUBMATCHES_PER_LINE", Default: DefaultMaxSubmatchesPerLine, Min: 1, Max: 1_000_000}

	// AnalyzeWindowLinesSetting is RX_ANALYZE_WINDOW_LINES. Its maximum
	// is the size of the detectors' fixed window.
	AnalyzeWindowLinesSetting = IntSetting{Name: "RX_ANALYZE_WINDOW_LINES", Default: DefaultAnalyzeWindowLines, Min: 1, Max: MaxAnalyzeWindowLines}

	// TaskTTLMinutesSetting is RX_TASK_TTL_MINUTES. Its maximum, one
	// week, keeps the duration far from overflowing time.Duration.
	TaskTTLMinutesSetting = IntSetting{Name: "RX_TASK_TTL_MINUTES", Default: DefaultTaskTTLMinutes, Min: 1, Max: 7 * 24 * 60}

	// SamplesWaitSecondsSetting is RX_SAMPLES_WAIT_SECONDS. 0 is
	// accepted: a lookup that needs an index answers 202 at once.
	SamplesWaitSecondsSetting = IntSetting{Name: "RX_SAMPLES_WAIT_SECONDS", Default: DefaultSamplesWaitSeconds, Min: 0, Max: 3600}

	// TimestampLookbackKBSetting is RX_TIMESTAMP_LOOKBACK_KB: how far
	// back, in KiB, a line without a timestamp of its own looks for the
	// line whose timestamp it carries in a samples answer. 0 is
	// accepted: such a line then carries none. Its maximum, 1 MiB,
	// bounds the read back from each sample's first line.
	TimestampLookbackKBSetting = IntSetting{Name: "RX_TIMESTAMP_LOOKBACK_KB", Default: DefaultTimestampLookbackKB, Min: 0, Max: 1024}

	// SamplesMaxLinesSetting is RX_SAMPLES_MAX_LINES: the most lines one
	// GET /v1/samples answer may hold, summed over its samples. A
	// request whose answer would hold more is refused with 400, so one
	// request (a thousand ranges open to the end of the file) cannot
	// make the server hold gigabytes. `rx samples` has no limit: it
	// runs as the user's own process.
	SamplesMaxLinesSetting = IntSetting{Name: "RX_SAMPLES_MAX_LINES", Default: DefaultSamplesMaxLines, Min: 1000, Max: 10_000_000}

	// SamplesMaxBytesSetting is RX_SAMPLES_MAX_BYTES: the most bytes of
	// line text one GET /v1/samples answer may hold, summed over its
	// samples. samples returns whole lines and a log's line can be
	// megabytes long, so a line count alone does not bound the memory
	// of an answer. Its maximum, 16 GiB, needs a 64-bit int, which
	// every platform rx is built for has.
	SamplesMaxBytesSetting = IntSetting{Name: "RX_SAMPLES_MAX_BYTES", Default: DefaultSamplesMaxBytes, Min: 1 << 20, Max: 16 << 30}

	// SamplesHeadMBSetting is RX_SAMPLES_HEAD_MB: how many MiB of a
	// file's text, from its first byte, a samples lookup reads to answer
	// without a line index when the file wants one and has none. A
	// lookup whose lines lie in that head is answered at once, and the
	// index is built in the background (`rx serve`) or not at all
	// (`rx samples`); any other waits for the build as before. 0 is
	// accepted: the lookup never answers early. Its maximum, 4 GiB,
	// bounds what one early answer may read.
	SamplesHeadMBSetting = IntSetting{Name: "RX_SAMPLES_HEAD_MB", Default: DefaultSamplesHeadMB, Min: 0, Max: 4096}

	// MaxIndexBuildsSetting is RX_MAX_INDEX_BUILDS: how many line-index
	// builds that GET /v1/samples starts run at once in `rx serve`.
	// Each reads a whole file, so the limit bounds the disk and CPU
	// that lookups across a tree of large files can set going; a build
	// past it waits in a queue. Its maximum bounds them whatever the
	// environment asks for.
	MaxIndexBuildsSetting = IntSetting{Name: "RX_MAX_INDEX_BUILDS", Default: DefaultMaxIndexBuilds, Min: 1, Max: 64}

	// ChainOverlapSecondsSetting is RX_CHAIN_OVERLAP_SECONDS: how far, in
	// seconds, the highest timestamp of a part of a log chain may be
	// after the first timestamp of the next part before the chain is
	// invalid (an overlap). It covers a program that writes to a rotated
	// file for a moment after the rotation. 0 is accepted: no overlap at
	// all. Its maximum is one day.
	ChainOverlapSecondsSetting = IntSetting{Name: "RX_CHAIN_OVERLAP_SECONDS", Default: DefaultChainOverlapSeconds, Min: 0, Max: 86400}
)

// DefaultChainOverlapSeconds is the default of RX_CHAIN_OVERLAP_SECONDS.
const DefaultChainOverlapSeconds = 60

// ChainOverlapMs returns RX_CHAIN_OVERLAP_SECONDS, from 0 to 86400
// seconds, or DefaultChainOverlapSeconds, in milliseconds.
func ChainOverlapMs() int64 { return int64(ChainOverlapSecondsSetting.Value()) * 1000 }

// DefaultMaxIndexBuilds is the default of RX_MAX_INDEX_BUILDS.
const DefaultMaxIndexBuilds = 2

// MaxIndexBuilds returns RX_MAX_INDEX_BUILDS, from 1 to 64, or
// DefaultMaxIndexBuilds.
func MaxIndexBuilds() int { return MaxIndexBuildsSetting.Value() }

// DefaultSamplesHeadMB is the default of RX_SAMPLES_HEAD_MB: 64 MiB,
// several hundred thousand lines of a typical log, which a plain file
// gives in tens of milliseconds and a compressed one in a few tenths of
// a second.
const DefaultSamplesHeadMB = 64

// SamplesHeadBytes returns RX_SAMPLES_HEAD_MB, from 0 to 4096 MiB, in
// bytes, or DefaultSamplesHeadMB in bytes. 0 means a samples lookup
// never answers from the head.
func SamplesHeadBytes() int64 { return int64(SamplesHeadMBSetting.Value()) << 20 }

// DefaultSamplesMaxLines is the default of RX_SAMPLES_MAX_LINES.
const DefaultSamplesMaxLines = 100_000

// DefaultSamplesMaxBytes is the default of RX_SAMPLES_MAX_BYTES: 256 MiB.
const DefaultSamplesMaxBytes = 256 << 20

// SamplesMaxLines returns RX_SAMPLES_MAX_LINES, from 1000 to 10000000
// lines, or DefaultSamplesMaxLines.
func SamplesMaxLines() int { return SamplesMaxLinesSetting.Value() }

// SamplesMaxBytes returns RX_SAMPLES_MAX_BYTES, from 1 MiB to 16 GiB, or
// DefaultSamplesMaxBytes.
func SamplesMaxBytes() int64 { return int64(SamplesMaxBytesSetting.Value()) }

// IntSettings is every integer setting rx reads, in the order the
// documentation lists them.
var IntSettings = []IntSetting{
	WorkersSetting,
	MaxSubprocessesSetting,
	MinChunkSizeMBSetting,
	LargeFileMBSetting,
	MaxLineTextBytesSetting,
	MaxSubmatchesPerLineSetting,
	AnalyzeWindowLinesSetting,
	TaskTTLMinutesSetting,
	SamplesWaitSecondsSetting,
	TimestampLookbackKBSetting,
	SamplesMaxLinesSetting,
	SamplesMaxBytesSetting,
	SamplesHeadMBSetting,
	MaxIndexBuildsSetting,
	ChainOverlapSecondsSetting,
}

// Value returns the setting's value from the environment, by the rule
// on IntSetting. It reads the environment on every call, so a test's
// t.Setenv takes effect at once.
func (s IntSetting) Value() int {
	raw := os.Getenv(s.Name)
	if raw == "" {
		return s.Default
	}
	value, problem := s.parse(raw)
	if problem != "" {
		warnInvalidSetting(s, raw, problem, value)
	}
	return value
}

// parse applies the rule to raw, a non-empty value, and returns the
// value to use and, when raw is not accepted as it is, why.
func (s IntSetting) parse(raw string) (value int, problem string) {
	n, err := strconv.Atoi(raw)
	switch {
	case err != nil:
		return s.Default, "not a whole number"
	case n < s.Min:
		return s.Default, "below the minimum"
	case n > s.Max:
		return s.Max, "above the maximum"
	default:
		return n, ""
	}
}

// warnedSettings holds the "name=value" pairs already reported, so each
// unacceptable value is logged once per process rather than on every
// read. A sync.Map is safe for the concurrent requests of `rx serve`.
// It holds one entry per variable and value the process's environment
// has held, which the operator sets and no request can change.
var warnedSettings sync.Map

// warnInvalidSetting logs one invalid_setting warning for s holding
// raw, unless this process has already logged it.
func warnInvalidSetting(s IntSetting, raw, problem string, used int) {
	// LoadOrStore stores the key and returns loaded=false for exactly
	// one caller, however many race here at once; that caller warns.
	if _, loaded := warnedSettings.LoadOrStore(s.Name+"="+raw, struct{}{}); loaded {
		return
	}
	slog.Default().Warn("invalid_setting",
		"name", s.Name,
		"value", raw,
		"problem", problem,
		"accepted", fmt.Sprintf("%d to %d", s.Min, s.Max),
		"using", used,
	)
}
