package config

import "time"

// DefaultSamplesIndexWait is how long GET /v1/samples waits for the line
// index of a file it is building before it answers 202 with the build's
// task instead of the lines. A few seconds covers the files an index
// builds for quickly, so most first lookups still answer in one round
// trip, while a multi-gigabyte file does not hold the request open for
// the minutes its index takes.
const DefaultSamplesIndexWait = 5 * time.Second

// SamplesIndexWait returns RX_SAMPLES_WAIT_SECONDS as a duration, or
// DefaultSamplesIndexWait when it is unset, negative or not a whole
// number. 0 is accepted: every lookup that needs an index built answers
// 202 at once.
func SamplesIndexWait() time.Duration {
	seconds := GetIntEnv("RX_SAMPLES_WAIT_SECONDS", -1)
	if seconds < 0 {
		return DefaultSamplesIndexWait
	}
	return time.Duration(seconds) * time.Second
}
