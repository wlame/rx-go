package config

// DefaultTimestampLookbackKB is the default of RX_TIMESTAMP_LOOKBACK_KB.
// 64 KiB holds a long Java stack trace after the line that logged it,
// and is a small read when a sample starts in the middle of one.
const DefaultTimestampLookbackKB = 64

// TimestampLookbackBytes returns RX_TIMESTAMP_LOOKBACK_KB, from 0 to
// 1024 KiB, in bytes, or DefaultTimestampLookbackKB in bytes. A line
// without a timestamp of its own carries the timestamp of the nearest
// earlier line that has one when that line starts at most this many
// bytes before it.
func TimestampLookbackBytes() int64 {
	return int64(TimestampLookbackKBSetting.Value()) * 1024
}
