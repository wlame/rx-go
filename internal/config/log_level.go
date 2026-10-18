package config

import (
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
)

// LogLevelName is the environment variable that sets the level rx logs
// at: DEBUG, INFO, WARN (or WARNING) or ERROR, in any case. INFO when it
// is unset.
const LogLevelName = "RX_LOG_LEVEL"

// logLevels maps each accepted value of RX_LOG_LEVEL, upper-cased, to
// its level. WARNING is Python's spelling of WARN, kept for rx-python's
// operators.
var logLevels = map[string]slog.Level{
	"DEBUG":   slog.LevelDebug,
	"INFO":    slog.LevelInfo,
	"WARN":    slog.LevelWarn,
	"WARNING": slog.LevelWarn,
	"ERROR":   slog.LevelError,
}

// logLevelNames spells each level as /health reports it.
var logLevelNames = map[slog.Level]string{
	slog.LevelDebug: "DEBUG",
	slog.LevelInfo:  "INFO",
	slog.LevelWarn:  "WARN",
	slog.LevelError: "ERROR",
}

// logLevelInForce is the level SetLogLevel last applied, as the int64 a
// slog.Level is; its zero value is slog.LevelInfo, the level of the
// default handler before any call.
var logLevelInForce atomic.Int64

// ApplyLogLevel reads RX_LOG_LEVEL, applies it to the default logger
// (SetLogLevel) and returns it. A value that is not a level is reported
// once with an invalid_setting warning, and INFO is used.
//
// The root command calls it before any subcommand runs, so every
// command honors the variable: `rx serve` quiets its per-request lines
// under ERROR, and `RX_LOG_LEVEL=DEBUG` shows the records the hooks
// dispatcher writes at Debug.
func ApplyLogLevel() slog.Level {
	raw := os.Getenv(LogLevelName)
	level, ok := logLevels[strings.ToUpper(strings.TrimSpace(raw))]
	if raw == "" {
		level, ok = slog.LevelInfo, true
	}
	if !ok {
		level = slog.LevelInfo
		warnInvalidLogLevel(raw)
	}
	SetLogLevel(level)
	return level
}

// SetLogLevel makes level the level of the default logger and the one
// LogLevelName reports. The default logger is slog's bridge to the log
// package (nothing in rx replaces it), whose level
// slog.SetLogLoggerLevel sets; the line format stays as it is.
func SetLogLevel(level slog.Level) {
	slog.SetLogLoggerLevel(level)
	logLevelInForce.Store(int64(level))
}

// LogLevelInForce returns the name of the level the default logger
// uses, as /health reports it under constants.LOG_LEVEL: DEBUG, INFO,
// WARN or ERROR.
func LogLevelInForce() string {
	return logLevelNames[slog.Level(logLevelInForce.Load())]
}

// warnInvalidLogLevel logs one invalid_setting warning for a value of
// RX_LOG_LEVEL that is not a level, once per process and value, as the
// integer settings do for theirs.
func warnInvalidLogLevel(raw string) {
	if _, loaded := warnedSettings.LoadOrStore(LogLevelName+"="+raw, struct{}{}); loaded {
		return
	}
	slog.Default().Warn("invalid_setting",
		"name", LogLevelName,
		"value", raw,
		"problem", "not a log level",
		"accepted", "DEBUG, INFO, WARN, ERROR",
		"using", "INFO",
	)
}
