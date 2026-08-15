package webapi

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wlame/rx-go/internal/prometheus"
)

// errorTypeByStatus maps an HTTP status onto the rx_errors_total
// error_type label. The vocabulary is rx-python's — see its
// prom.record_error call sites in src/rx/web.py — so one alert rule and
// one dashboard panel work against either backend.
var errorTypeByStatus = map[int]string{
	http.StatusBadRequest:          "invalid_params",
	http.StatusForbidden:           "access_denied",
	http.StatusNotFound:            "file_not_found",
	http.StatusConflict:            "invalid_params",
	http.StatusServiceUnavailable:  "service_unavailable",
	http.StatusInternalServerError: "internal_error",
}

// metricErrorTyper is implemented by an error that knows which
// rx_errors_total bucket it belongs in, for the cases the status code
// cannot distinguish.
type metricErrorTyper interface {
	MetricErrorType() string
}

// errorTypeOf classifies err for the rx_errors_total label. An error
// that names its own type wins; otherwise the status code decides.
// Anything unrecognized counts as internal_error rather than going
// uncounted — a silent bucket is worse than a coarse one.
func errorTypeOf(err error) string {
	var typed metricErrorTyper
	if errors.As(err, &typed) {
		if t := typed.MetricErrorType(); t != "" {
			return t
		}
	}
	var status huma.StatusError
	if errors.As(err, &status) {
		if t, ok := errorTypeByStatus[status.GetStatus()]; ok {
			return t
		}
	}
	return "internal_error"
}

// recordEndpoint counts one request against an endpoint counter, and on
// failure against rx_errors_total as well.
//
// Handlers call this from a single deferred site rather than at each
// return, so a request is counted exactly once no matter which of the
// handler's many exit paths it takes — the shape that let the counters
// fall out of use in the first place.
//
// count is the per-endpoint helper (prometheus.RecordTraceRequest and
// friends); the status label is "success" or "error", as rx-python
// spells it.
func recordEndpoint(count func(status string), err error) {
	if err != nil {
		count("error")
		prometheus.RecordError(errorTypeOf(err))
		return
	}
	count("success")
}

// pathKindByExtension maps a file extension onto the trace-duration
// path_kind label. The three kinds have order-of-magnitude different
// costs, so one undifferentiated latency histogram would hide which
// kind a slow request hit.
var pathKindByExtension = map[string]string{
	".zst":  "seekable",
	".zstd": "seekable",
	".gz":   "compressed",
	".bz2":  "compressed",
	".xz":   "compressed",
	".lz4":  "compressed",
}

// pathKindOf labels a request by the most expensive path it touches: a
// request that reads one compressed file among ten plain ones is paced
// by the compressed one.
func pathKindOf(paths []string) string {
	for _, p := range paths {
		if kind, ok := pathKindByExtension[strings.ToLower(filepath.Ext(p))]; ok {
			return kind
		}
	}
	return "regular"
}

// observeTraceDuration records how long a successful trace took.
func observeTraceDuration(paths []string, start time.Time) {
	prometheus.RecordTraceDuration(pathKindOf(paths), time.Since(start))
}
