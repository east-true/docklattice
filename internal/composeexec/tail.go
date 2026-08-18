package composeexec

import "sync"

const DefaultTailBytes = 64 << 10

// TailWriter retains only the newest bytes and is safe for concurrent stdout
// and stderr drain goroutines.
type TailWriter struct {
	mu        sync.Mutex
	buffer    []byte
	start     int
	size      int
	total     uint64
	truncated bool
}

func NewTailWriter(capacity int) *TailWriter {
	if capacity <= 0 {
		capacity = DefaultTailBytes
	}
	return &TailWriter{buffer: make([]byte, capacity)}
}

func (writer *TailWriter) Write(payload []byte) (int, error) {
	written := len(payload)
	writer.mu.Lock()
	defer writer.mu.Unlock()
	writer.total += uint64(written)
	if written == 0 {
		return 0, nil
	}
	if written >= len(writer.buffer) {
		copy(writer.buffer, payload[written-len(writer.buffer):])
		writer.start = 0
		writer.size = len(writer.buffer)
		writer.truncated = writer.total > uint64(len(writer.buffer))
		return written, nil
	}
	if overflow := writer.size + written - len(writer.buffer); overflow > 0 {
		writer.start = (writer.start + overflow) % len(writer.buffer)
		writer.size -= overflow
		writer.truncated = true
	}
	end := (writer.start + writer.size) % len(writer.buffer)
	first := written
	if untilWrap := len(writer.buffer) - end; first > untilWrap {
		first = untilWrap
	}
	copy(writer.buffer[end:end+first], payload[:first])
	copy(writer.buffer[:written-first], payload[first:])
	writer.size += written
	return written, nil
}

func (writer *TailWriter) Bytes() []byte {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	out := make([]byte, writer.size)
	if writer.size == 0 {
		return out
	}
	first := writer.size
	if remaining := len(writer.buffer) - writer.start; first > remaining {
		first = remaining
	}
	copy(out, writer.buffer[writer.start:writer.start+first])
	copy(out[first:], writer.buffer[:writer.size-first])
	return out
}

func (writer *TailWriter) Truncated() bool {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.truncated
}

func (writer *TailWriter) TotalBytes() uint64 {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.total
}
