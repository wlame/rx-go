package webapi

import (
	"testing"

	"github.com/wlame/rx-go/internal/prometheus"
)

// rx_large_file_threshold_mb publishes the large-file threshold a
// scrape needs to read the cache and index families by: the value of
// RX_LARGE_FILE_MB, not the chunk size.
func TestEnableMetrics_PublishesTheLargeFileThreshold(t *testing.T) {
	t.Setenv("RX_LARGE_FILE_MB", "123")
	t.Setenv("RX_MIN_CHUNK_SIZE_MB", "7")
	t.Cleanup(prometheus.Disable)

	enableMetrics()

	families, err := prometheus.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != "rx_large_file_threshold_mb" {
			continue
		}
		if got := family.GetMetric()[0].GetGauge().GetValue(); got != 123 {
			t.Errorf("rx_large_file_threshold_mb = %v, want 123 from RX_LARGE_FILE_MB", got)
		}
		return
	}
	t.Fatal("rx_large_file_threshold_mb is not registered")
}
