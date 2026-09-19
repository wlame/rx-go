package config

import "time"

// DefaultSamplesIndexWait is how long GET /v1/samples waits for the line
// index of a file it is building before it answers 202 with the build's
// task instead of the lines. A few seconds covers the files an index
// builds for quickly, so most first lookups still answer in one round
// trip, while a multi-gigabyte file does not hold the request open for
// the minutes its index takes.
const DefaultSamplesIndexWait = DefaultSamplesWaitSeconds * time.Second

// DefaultSamplesWaitSeconds is DefaultSamplesIndexWait in seconds, the
// default of RX_SAMPLES_WAIT_SECONDS.
const DefaultSamplesWaitSeconds = 5

// SamplesIndexWait returns RX_SAMPLES_WAIT_SECONDS, from 0 to 3600
// seconds, as a duration, or DefaultSamplesIndexWait. 0 is accepted:
// every lookup that needs an index built answers 202 at once.
func SamplesIndexWait() time.Duration {
	return time.Duration(SamplesWaitSecondsSetting.Value()) * time.Second
}
