package auditwal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"
)

func TestAppendOnceReturnsOneCursorAcrossRetryAndReopen(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	options := testOptions()
	wal, err := Open(directory, "agent-once", 1, options)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"operation_id":"op-1"}`)
	first, err := wal.AppendOnce(context.Background(), "managed-operation:op-1", payload)
	if err != nil {
		t.Fatal(err)
	}
	second, err := wal.AppendOnce(context.Background(), "managed-operation:op-1", payload)
	if err != nil {
		t.Fatal(err)
	}
	if second.Cursor != first.Cursor {
		t.Fatalf("retry cursor=%+v first=%+v", second.Cursor, first.Cursor)
	}
	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(directory, "agent-once", 1, options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	third, err := reopened.AppendOnce(context.Background(), "managed-operation:op-1", payload)
	if err != nil {
		t.Fatal(err)
	}
	if third.Cursor != first.Cursor {
		t.Fatalf("reopen cursor=%+v first=%+v", third.Cursor, first.Cursor)
	}
	read, err := reopened.ReadAuditFrom(context.Background(), Cursor{Incarnation: 1, Seq: 1}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Records) != 1 {
		t.Fatalf("records=%d, want exactly one", len(read.Records))
	}
}

func TestAppendOnceRecoversFrameSyncedBeforeReceiptCompletion(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	options := testOptions()
	wal, err := Open(directory, "agent-crash-window", 1, options)
	if err != nil {
		t.Fatal(err)
	}
	key := "managed-operation:crash-window"
	payload := []byte(`{"operation_id":"crash-window"}`)
	first, err := wal.AppendOnce(context.Background(), key, payload)
	if err != nil {
		t.Fatal(err)
	}

	// Recreate the only ambiguous crash window: the frame is durable but the
	// receipt still says RESERVED. A restart must find the frame, not append.
	digest := sha256.Sum256(payload)
	wal.mu.Lock()
	err = wal.saveOnceReceiptLocked(onceReceipt{
		Version: onceReceiptVersion, Key: key,
		PayloadSHA256: hex.EncodeToString(digest[:]),
	})
	wal.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(directory, "agent-crash-window", 1, options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recovered, err := reopened.AppendOnce(context.Background(), key, payload)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Cursor != first.Cursor || recovered.AppendedAt.IsZero() {
		t.Fatalf("recovered=%+v first=%+v", recovered, first)
	}
	if bounds, err := reopened.Bounds(); err != nil || bounds.NextCursor != (Cursor{Incarnation: 1, Seq: 2}) {
		t.Fatalf("bounds=%+v err=%v", bounds, err)
	}
}

func TestAppendOnceRejectsKeyPayloadMismatchAndForgetIsIdempotent(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	wal, err := Open(directory, "agent-mismatch", 1, Options{
		MaxBytes: 1 << 20, MaxAge: time.Hour, SyncInterval: time.Hour, SyncBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer wal.Close()
	if _, err := wal.AppendOnce(context.Background(), "key", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := wal.AppendOnce(context.Background(), "key", []byte("two")); err == nil {
		t.Fatal("reused key with changed payload was accepted")
	}
	if err := wal.ForgetOnce("key"); err != nil {
		t.Fatal(err)
	}
	if err := wal.ForgetOnce("key"); err != nil {
		t.Fatal(err)
	}
}
