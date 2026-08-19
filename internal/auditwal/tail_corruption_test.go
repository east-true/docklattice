package auditwal

import (
	"context"
	"os"
	"testing"
)

// A crash can leave the last segment extended with zeros: the file size grew
// but the data blocks never reached the device. That tail carries no record
// and must be repaired like any other torn tail.
func TestZeroFilledTailIsRepairedNotFatal(t *testing.T) {
	t.Parallel()
	for _, padding := range []int{4, 8, 64, 4096} {
		dir := t.TempDir()
		options := testOptions()
		options.SyncBytes = 1 << 20
		wal := openTestWAL(t, dir, 1, options)
		if _, err := wal.Append(context.Background(), []byte("keep")); err != nil {
			t.Fatal(err)
		}
		if err := wal.Close(); err != nil {
			t.Fatal(err)
		}
		path := segmentPaths(t, dir)[0]
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(make([]byte, padding)); err != nil {
			t.Fatal(err)
		}
		file.Close()

		recovery, err := Recover(dir, "agent-1", options)
		if err != nil {
			t.Fatalf("padding %d: Recover = %v", padding, err)
		}
		if recovery.WALTail == nil || *recovery.WALTail != (Cursor{1, 1}) {
			t.Fatalf("padding %d: tail = %+v", padding, recovery.WALTail)
		}
		reopened, err := Open(dir, "agent-1", 2, options)
		if err != nil {
			t.Fatalf("padding %d: Open = %v", padding, err)
		}
		read, err := reopened.ReadAuditFrom(context.Background(), Cursor{1, 1}, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(read.Records) != 1 || string(read.Records[0].Payload) != "keep" {
			t.Fatalf("padding %d: records = %+v", padding, read.Records)
		}
		if _, err := reopened.Append(context.Background(), []byte("after repair")); err != nil {
			t.Fatalf("padding %d: append after repair: %v", padding, err)
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// Zeros are only a repairable tail when nothing follows them. A hole with
// real bytes after it is media corruption and must stay fatal.
func TestZeroHoleFollowedByBytesStaysFatal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	options := testOptions()
	options.SyncBytes = 1 << 20
	wal := openTestWAL(t, dir, 1, options)
	if _, err := wal.Append(context.Background(), []byte("keep")); err != nil {
		t.Fatal(err)
	}
	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}
	path := segmentPaths(t, dir)[0]
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append(make([]byte, 64), 'x')); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, err := Recover(dir, "agent-1", options); err == nil {
		t.Fatal("a zero hole with bytes after it was silently truncated")
	}
}
