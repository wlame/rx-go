package compression

import (
	"bytes"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// TestDecoderPool_DecodesCorrectly asserts basic correctness — a pooled
// decoder round-trips compressed data.
func TestDecoderPool_DecodesCorrectly(t *testing.T) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	plaintext := []byte("hello, pool — some zstd-compressible payload with repetition repetition repetition")
	compressed := enc.EncodeAll(plaintext, nil)
	_ = enc.Close()

	dec := AcquireDecoder()
	defer ReleaseDecoder(dec)
	out, err := dec.DecodeAll(compressed, nil)
	if err != nil {
		t.Fatalf("DecodeAll: %v", err)
	}
	if !bytes.Equal(out, plaintext) {
		t.Errorf("round trip mismatch:\n got:  %q\n want: %q", out, plaintext)
	}
}

// TestDecoderPool_ConcurrentSafe asserts the pool is safe under -race
// with many concurrent users. Any data race (or double-use of the same
// decoder from two goroutines simultaneously) would fail under -race.
func TestDecoderPool_ConcurrentSafe(t *testing.T) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	plaintext := bytes.Repeat([]byte("a"), 4096)
	compressed := enc.EncodeAll(plaintext, nil)
	_ = enc.Close()

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				dec := AcquireDecoder()
				out, err := dec.DecodeAll(compressed, nil)
				if err != nil {
					t.Errorf("concurrent DecodeAll: %v", err)
					ReleaseDecoder(dec)
					return
				}
				if !bytes.Equal(out, plaintext) {
					t.Errorf("concurrent round trip mismatch (len got=%d want=%d)", len(out), len(plaintext))
					ReleaseDecoder(dec)
					return
				}
				ReleaseDecoder(dec)
			}
		}()
	}
	wg.Wait()
}

// TestReleaseDecoder_NilIsSafe — passing a nil decoder must be a no-op.
// Defensive-programming guard so Acquire-failure paths don't need to
// track whether they actually got a decoder.
func TestReleaseDecoder_NilIsSafe(t *testing.T) {
	// No assertion other than "does not panic".
	ReleaseDecoder(nil)
}
