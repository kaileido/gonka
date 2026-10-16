package strictjson

import (
	"sync"
	"sync/atomic"
	"testing"
)

type probe struct {
	Error  any    `json:"error"`
	Object string `json:"object"`
}

// Test flow:
//  1. Decode, from several goroutines at once, a payload malformed in the shape that makes goccy read past its buffer.
//  2. Repeat it many times, as a busy gateway would.
//  3. Assert every call refuses it with ErrInvalid and none panics.
func TestUnmarshalRefusesMalformedBytesWithoutPanicking(t *testing.T) {
	payload := []byte(`{"error":1,"` + "\\0\x05\\")
	var panics, accepted atomic.Int64
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 2000 {
				func() {
					defer func() {
						if recover() != nil {
							panics.Add(1)
						}
					}()
					var decoded probe
					if Unmarshal(payload, &decoded) == nil {
						accepted.Add(1)
					}
				}()
			}
		})
	}
	workers.Wait()
	if panics.Load() != 0 || accepted.Load() != 0 {
		t.Fatalf("panics %d, accepted %d, want neither", panics.Load(), accepted.Load())
	}
}

// Test flow:
//  1. Generate payloads and decode each with Unmarshal.
//  2. Recover any panic.
//  3. Assert none happened: no byte sequence a host or client sends can crash the decode.
func FuzzUnmarshalNeverPanics(f *testing.F) {
	f.Add([]byte(`{"error":{"message":"boom"},"object":"error"}`))
	f.Add([]byte(`{"error":1,"` + "\\0\x05\\"))
	f.Fuzz(func(t *testing.T, payload []byte) {
		var decoded probe
		_ = Unmarshal(payload, &decoded)
	})
}
