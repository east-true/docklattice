package composeexec

import (
	"bytes"
	"sync"
	"testing"
)

func TestTailWriterRetainsNewestBytesAcrossWrap(t *testing.T) {
	writer := NewTailWriter(8)
	for _, payload := range []string{"abc", "defg", "hijkl"} {
		if count, err := writer.Write([]byte(payload)); err != nil || count != len(payload) {
			t.Fatalf("Write(%q) = %d, %v", payload, count, err)
		}
	}
	if got := string(writer.Bytes()); got != "efghijkl" {
		t.Fatalf("tail = %q, want efghijkl", got)
	}
	if !writer.Truncated() || writer.TotalBytes() != 12 {
		t.Fatalf("tail metadata: truncated=%t total=%d", writer.Truncated(), writer.TotalBytes())
	}
	if _, err := writer.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if got := string(writer.Bytes()); got != "23456789" {
		t.Fatalf("oversized write tail = %q", got)
	}
}

func TestTailWriterConcurrentStdoutStderrIsBounded(t *testing.T) {
	writer := NewTailWriter(1024)
	var group sync.WaitGroup
	for index := 0; index < 16; index++ {
		group.Add(1)
		go func(value byte) {
			defer group.Done()
			for count := 0; count < 100; count++ {
				_, _ = writer.Write(bytes.Repeat([]byte{value}, 37))
			}
		}(byte(index))
	}
	group.Wait()
	if len(writer.Bytes()) != 1024 || !writer.Truncated() || writer.TotalBytes() != 16*100*37 {
		t.Fatalf("concurrent tail: len=%d truncated=%t total=%d", len(writer.Bytes()), writer.Truncated(), writer.TotalBytes())
	}
}

func TestNonBlockingRelayEmitsDropMarkerWhenCapacityReturns(t *testing.T) {
	output := make(chan OutputChunk, 1)
	relay := &nonBlockingRelay{channel: output}
	relay.send(StreamStdout, []byte("first"))
	relay.send(StreamStdout, []byte("dropped"))
	if got := <-output; string(got.Data) != "first" {
		t.Fatalf("first chunk = %+v", got)
	}
	relay.send(StreamStderr, []byte("next"))
	marker := <-output
	if marker.DroppedBytes != uint64(len("dropped")) || len(marker.Data) != 0 {
		t.Fatalf("drop marker = %+v", marker)
	}
	if relay.totalDropped.Load() != uint64(len("dropped")+len("next")) {
		t.Fatalf("total dropped = %d", relay.totalDropped.Load())
	}
}
