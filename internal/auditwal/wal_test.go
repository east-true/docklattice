package auditwal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestDefaultPolicyMatchesArchitecture(t *testing.T) {
	options := DefaultOptions()
	if options.MaxBytes != 256<<20 || options.MaxAge != 14*24*time.Hour {
		t.Fatalf("retention defaults = %d/%v", options.MaxBytes, options.MaxAge)
	}
	if options.SyncInterval != time.Second || options.SyncBytes != 64<<10 {
		t.Fatalf("sync defaults = %v/%d", options.SyncInterval, options.SyncBytes)
	}
}

func TestAppendReadIdentityAndReopenAcrossIncarnations(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wal := openTestWAL(t, dir, 1, testOptions())
	first, err := wal.Append(context.Background(), []byte("one"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := wal.Append(context.Background(), []byte("two"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Cursor != (Cursor{1, 1}) || second.Cursor != (Cursor{1, 2}) {
		t.Fatalf("cursors = %+v %+v", first.Cursor, second.Cursor)
	}
	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}

	w2 := openTestWAL(t, dir, 2, testOptions())
	third, err := w2.Append(context.Background(), []byte("three"))
	if err != nil {
		t.Fatal(err)
	}
	if third.Cursor != (Cursor{2, 1}) {
		t.Fatalf("third cursor = %+v", third.Cursor)
	}
	if err := w2.Sync(); err != nil {
		t.Fatal(err)
	}
	read, err := w2.ReadAuditFrom(context.Background(), Cursor{1, 1}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if read.BehindFloor != nil || len(read.Records) != 3 {
		t.Fatalf("read = %+v", read)
	}
	for index, want := range []string{"one", "two", "three"} {
		if read.Records[index].AgentID != "agent-1" || string(read.Records[index].Payload) != want {
			t.Fatalf("record %d = %+v", index, read.Records[index])
		}
	}
	bounds, err := w2.Bounds()
	if err != nil {
		t.Fatal(err)
	}
	if bounds.NextCursor != (Cursor{2, 2}) || bounds.DurableThrough == nil || *bounds.DurableThrough != (Cursor{2, 1}) {
		t.Fatalf("bounds = %+v", bounds)
	}
}

func TestByteThresholdAndTimerFsyncProduceDurableCursor(t *testing.T) {
	t.Parallel()

	t.Run("bytes", func(t *testing.T) {
		options := testOptions()
		options.SyncBytes = 1
		wal := openTestWAL(t, t.TempDir(), 1, options)
		record, err := wal.Append(context.Background(), []byte("sync-now"))
		if err != nil {
			t.Fatal(err)
		}
		bounds, _ := wal.Bounds()
		if bounds.DurableThrough == nil || *bounds.DurableThrough != record.Cursor {
			t.Fatalf("durable cursor = %+v", bounds.DurableThrough)
		}
	})

	t.Run("timer", func(t *testing.T) {
		options := testOptions()
		options.SyncBytes = 1 << 20
		options.SyncInterval = 20 * time.Millisecond
		wal := openTestWAL(t, t.TempDir(), 1, options)
		record, err := wal.Append(context.Background(), []byte("timer"))
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(time.Second)
		for {
			bounds, _ := wal.Bounds()
			if bounds.DurableThrough != nil && *bounds.DurableThrough == record.Cursor {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("timer did not fsync within deadline")
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
}

func TestRecoveryTruncatesIncompleteAndChecksumBadTail(t *testing.T) {
	t.Parallel()

	t.Run("incomplete", func(t *testing.T) {
		dir := t.TempDir()
		wal := openTestWAL(t, dir, 1, testOptions())
		if _, err := wal.Append(context.Background(), []byte("valid")); err != nil {
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
		if _, err := file.Write([]byte{0, 0, 0, 20, 1, 2}); err != nil {
			t.Fatal(err)
		}
		file.Close()

		reopened := openTestWAL(t, dir, 2, testOptions())
		read, err := reopened.ReadAuditFrom(context.Background(), Cursor{1, 1}, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(read.Records) != 1 || string(read.Records[0].Payload) != "valid" {
			t.Fatalf("recovered records = %+v", read.Records)
		}
	})

	t.Run("checksum", func(t *testing.T) {
		dir := t.TempDir()
		options := testOptions()
		options.SyncBytes = 1 << 20
		wal := openTestWAL(t, dir, 1, options)
		if _, err := wal.Append(context.Background(), []byte("keep")); err != nil {
			t.Fatal(err)
		}
		if _, err := wal.Append(context.Background(), []byte("drop")); err != nil {
			t.Fatal(err)
		}
		if err := wal.Close(); err != nil {
			t.Fatal(err)
		}
		path := segmentPaths(t, dir)[0]
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data[len(data)-1] ^= 0xff
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}

		reopened := openTestWAL(t, dir, 2, options)
		read, err := reopened.ReadAuditFrom(context.Background(), Cursor{1, 1}, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(read.Records) != 1 || string(read.Records[0].Payload) != "keep" {
			t.Fatalf("checksum recovery = %+v", read.Records)
		}
	})

	t.Run("checksum corruption before a later frame is not a repairable tail", func(t *testing.T) {
		dir := t.TempDir()
		options := testOptions()
		options.SyncBytes = 1 << 20
		wal := openTestWAL(t, dir, 1, options)
		first, _ := encodeFrame(Cursor{1, 1}, options.Now(), []byte("first"))
		if _, err := wal.Append(context.Background(), []byte("first")); err != nil {
			t.Fatal(err)
		}
		if _, err := wal.Append(context.Background(), []byte("second")); err != nil {
			t.Fatal(err)
		}
		if err := wal.Close(); err != nil {
			t.Fatal(err)
		}
		path := segmentPaths(t, dir)[0]
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data[len(first)-1] ^= 0xff
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, "agent-1", 2, options); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Open error = %v", err)
		}
	})
}

func TestRecoverProvidesTailBeforeNextIncarnationStartup(t *testing.T) {
	t.Parallel()

	empty, err := Recover(filepath.Join(t.TempDir(), "not-created"), "agent-1", testOptions())
	if err != nil || empty.WALTail != nil || empty.DurableThrough != nil {
		t.Fatalf("empty recovery = %+v, %v", empty, err)
	}
	dir := t.TempDir()
	wal := openTestWAL(t, dir, 1, testOptions())
	if _, err := wal.Append(context.Background(), []byte("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := wal.Append(context.Background(), []byte("two")); err != nil {
		t.Fatal(err)
	}
	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}
	recovery, err := Recover(dir, "agent-1", testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if recovery.WALTail == nil || *recovery.WALTail != (Cursor{1, 2}) ||
		recovery.DurableThrough == nil || *recovery.DurableThrough != (Cursor{1, 2}) {
		t.Fatalf("recovery = %+v", recovery)
	}
}

func TestCorruptionOutsideLastSegmentIsRejected(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	options := testOptions()
	options.SyncBytes = 1
	wal := openTestWAL(t, dir, 1, options)
	for _, payload := range []string{"first", "second"} {
		if _, err := wal.Append(context.Background(), []byte(payload)); err != nil {
			t.Fatal(err)
		}
	}
	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}
	paths := segmentPaths(t, dir)
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(paths[0], data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, "agent-1", 2, options); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open error = %v", err)
	}
}

func TestSizeAndAgeFirstHitRetentionCreateExactGaps(t *testing.T) {
	t.Parallel()

	t.Run("size", func(t *testing.T) {
		dir := t.TempDir()
		options := testOptions()
		options.SyncBytes = 1
		frame, _ := encodeFrame(Cursor{1, 1}, options.Now(), []byte("payload"))
		options.MaxBytes = int64(len(frame) * 2)
		wal := openTestWAL(t, dir, 1, options)
		for index := 0; index < 3; index++ {
			if _, err := wal.Append(context.Background(), []byte("payload")); err != nil {
				t.Fatal(err)
			}
		}
		coverage, _ := wal.GetAuditCoverage()
		if coverage.Revision != 1 || len(coverage.Gaps) != 1 {
			t.Fatalf("coverage = %+v", coverage)
		}
		gap := coverage.Gaps[0]
		if gap.Incarnation != 1 || gap.FromSeq != 1 || gap.UntilSeq != 2 || gap.Reason != GapRetention || gap.Precision != PrecisionExact {
			t.Fatalf("retention gap = %+v", gap)
		}
		read, _ := wal.ReadAuditFrom(context.Background(), Cursor{1, 1}, 10)
		if read.BehindFloor == nil || read.BehindFloor.Bounds.WALFloor == nil || *read.BehindFloor.Bounds.WALFloor != (Cursor{1, 2}) {
			t.Fatalf("behind-floor response = %+v", read.BehindFloor)
		}
	})

	t.Run("age", func(t *testing.T) {
		base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
		now := base
		options := testOptions()
		options.Now = func() time.Time { return now }
		options.SyncBytes = 1
		wal := openTestWAL(t, t.TempDir(), 1, options)
		if _, err := wal.Append(context.Background(), []byte("old")); err != nil {
			t.Fatal(err)
		}
		now = base.Add(14*24*time.Hour + time.Nanosecond)
		if _, err := wal.EnforceLimits(now); err != nil {
			t.Fatal(err)
		}
		coverage, _ := wal.GetAuditCoverage()
		if len(coverage.Gaps) != 1 || coverage.Gaps[0].Reason != GapRetention {
			t.Fatalf("age coverage = %+v", coverage)
		}
	})
}

func TestACKPersistenceDeletionAndBehindFloor(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	options := testOptions()
	options.SyncBytes = 1
	wal := openTestWAL(t, dir, 1, options)
	for _, payload := range []string{"one", "two"} {
		if _, err := wal.Append(context.Background(), []byte(payload)); err != nil {
			t.Fatal(err)
		}
	}
	if err := wal.RebindArchive("archive-1"); err != nil {
		t.Fatal(err)
	}
	if err := wal.AckAudit("archive-1", Cursor{1, 1}, 0); err != nil {
		t.Fatal(err)
	}
	bounds, _ := wal.Bounds()
	if bounds.ServerACKedThrough == nil || *bounds.ServerACKedThrough != (Cursor{1, 1}) {
		t.Fatalf("ACK bounds = %+v", bounds)
	}
	if bounds.WALFloor == nil || *bounds.WALFloor != (Cursor{1, 2}) {
		t.Fatalf("floor = %+v", bounds.WALFloor)
	}
	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openTestWAL(t, dir, 2, options)
	bounds, _ = reopened.Bounds()
	if bounds.AcknowledgedArchiveID != "archive-1" || bounds.ServerACKedThrough == nil || *bounds.ServerACKedThrough != (Cursor{1, 1}) {
		t.Fatalf("reopened ACK = %+v", bounds)
	}
	read, err := reopened.ReadAuditFrom(context.Background(), Cursor{1, 1}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if read.BehindFloor == nil {
		t.Fatal("expected CURSOR_BEHIND_FLOOR")
	}
}

func TestStaleCoverageBlocksACKUntilSnapshotRevisionSeen(t *testing.T) {
	t.Parallel()

	options := testOptions()
	options.SyncBytes = 1
	frame, _ := encodeFrame(Cursor{1, 1}, options.Now(), []byte("payload"))
	options.MaxBytes = int64(len(frame) * 2)
	wal := openTestWAL(t, t.TempDir(), 1, options)
	for index := 0; index < 3; index++ {
		if _, err := wal.Append(context.Background(), []byte("payload")); err != nil {
			t.Fatal(err)
		}
	}
	if err := wal.RebindArchive("archive"); err != nil {
		t.Fatal(err)
	}
	err := wal.AckAudit("archive", Cursor{1, 2}, 0)
	var stale *StaleCoverageError
	if !errors.As(err, &stale) || len(stale.BlockingRanges) != 1 || stale.CurrentRevision != 1 {
		t.Fatalf("stale error = %#v / %v", stale, err)
	}
	if err := wal.AckAudit("archive", Cursor{1, 2}, stale.CurrentRevision); err != nil {
		t.Fatal(err)
	}
	if err := wal.AckAudit("wrong", Cursor{1, 3}, stale.CurrentRevision); !errors.Is(err, ErrArchiveMismatch) {
		t.Fatalf("archive mismatch = %v", err)
	}
	if err := wal.AckAudit("archive", Cursor{1, 1}, stale.CurrentRevision); !errors.Is(err, ErrCursorRollback) {
		t.Fatalf("cursor rollback = %v", err)
	}
	if err := wal.AckAudit("archive", Cursor{1, 99}, stale.CurrentRevision); !errors.Is(err, ErrCursorAhead) {
		t.Fatalf("cursor ahead = %v", err)
	}
}

func TestDiskPressureProtectsActiveThenCreatesDurableGap(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	options := testOptions()
	options.SyncBytes = 1 << 20
	wal := openTestWAL(t, dir, 1, options)
	if _, err := wal.Append(context.Background(), []byte("active")); err != nil {
		t.Fatal(err)
	}
	result, err := wal.ReclaimForDiskPressure(1)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Degraded || result.FreedBytes != 0 || len(result.Coverage.Gaps) != 0 {
		t.Fatalf("active reclaim = %+v", result)
	}
	if err := wal.Sync(); err != nil {
		t.Fatal(err)
	}
	result, err = wal.ReclaimForDiskPressure(1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Degraded || result.DeletedUnackedBytes == 0 || len(result.Coverage.Gaps) != 1 || result.Coverage.Gaps[0].Reason != GapDiskPressure {
		t.Fatalf("closed reclaim = %+v", result)
	}
	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestWAL(t, dir, 2, options)
	coverage, _ := reopened.GetAuditCoverage()
	if len(coverage.Gaps) != 1 || coverage.Gaps[0].Reason != GapDiskPressure {
		t.Fatalf("reopened coverage = %+v", coverage)
	}
}

func TestSplitDiskPressureTiersDoNotLoseUnackedDataEarly(t *testing.T) {
	t.Parallel()

	options := testOptions()
	options.SyncBytes = 1 << 20
	wal := openTestWAL(t, t.TempDir(), 1, options)
	if _, err := wal.Append(context.Background(), []byte("unacked")); err != nil {
		t.Fatal(err)
	}
	if err := wal.Sync(); err != nil {
		t.Fatal(err)
	}

	acked, err := wal.ReclaimACKedForDiskPressure(1)
	if err != nil {
		t.Fatal(err)
	}
	if acked.FreedBytes != 0 || acked.DeletedUnackedBytes != 0 || len(acked.Coverage.Gaps) != 0 || !acked.Degraded {
		t.Fatalf("ACKed tier crossed into unACKed data: %+v", acked)
	}

	unacked, err := wal.ReclaimUnackedForDiskPressure(1)
	if err != nil {
		t.Fatal(err)
	}
	if unacked.DeletedACKedBytes != 0 || unacked.DeletedUnackedBytes == 0 || unacked.Degraded ||
		len(unacked.Coverage.Gaps) != 1 || unacked.Coverage.Gaps[0].Reason != GapDiskPressure {
		t.Fatalf("unACKed tier = %+v", unacked)
	}
}

func TestConcurrentAppendAllocatesUniqueContiguousSequence(t *testing.T) {
	t.Parallel()

	options := testOptions()
	options.SyncBytes = 1 << 20
	wal := openTestWAL(t, t.TempDir(), 1, options)
	const count = 100
	cursors := make(chan Cursor, count)
	errs := make(chan error, count)
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			record, err := wal.Append(context.Background(), []byte("event"))
			if err != nil {
				errs <- err
				return
			}
			cursors <- record.Cursor
		}()
	}
	wait.Wait()
	close(cursors)
	close(errs)
	for err := range errs {
		t.Errorf("Append: %v", err)
	}
	seqs := make([]int, 0, count)
	for cursor := range cursors {
		seqs = append(seqs, int(cursor.Seq))
	}
	sort.Ints(seqs)
	if len(seqs) != count {
		t.Fatalf("cursor count = %d", len(seqs))
	}
	for index, seq := range seqs {
		if seq != index+1 {
			t.Fatalf("sequence[%d] = %d", index, seq)
		}
	}
}

func TestGapMergingAndACKIntersectionDoNotUseCrossIncarnationSubtraction(t *testing.T) {
	gaps := mergeGap([]Gap{{
		Incarnation: 1, FromSeq: 1, UntilSeq: 3, Reason: GapRetention,
		Precision: PrecisionExact, LastLossRevision: 1,
	}}, Gap{
		Incarnation: 1, FromSeq: 3, UntilSeq: 5, Reason: GapRetention,
		Precision: PrecisionExact, LastLossRevision: 2,
	})
	if len(gaps) != 1 || gaps[0].FromSeq != 1 || gaps[0].UntilSeq != 5 || gaps[0].LastLossRevision != 2 {
		t.Fatalf("merged gaps = %+v", gaps)
	}
	if !gapIntersectsACK(gaps[0], &Cursor{1, 2}, Cursor{2, 1}) {
		t.Fatal("cross-incarnation ACK did not intersect")
	}
	if gapIntersectsACK(gaps[0], &Cursor{1, 4}, Cursor{2, 1}) {
		t.Fatal("already ACKed gap intersected")
	}
}

func testOptions() Options {
	return Options{
		MaxBytes: 1 << 20, MaxAge: 14 * 24 * time.Hour,
		SyncInterval: time.Hour, SyncBytes: 1 << 20,
		Now: func() time.Time { return time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC) },
	}
}

func openTestWAL(t *testing.T, dir string, incarnation uint64, options Options) *WAL {
	t.Helper()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	wal, err := Open(dir, "agent-1", incarnation, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wal.Close() })
	return wal
}

func segmentPaths(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "segment-*.wal"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		t.Fatal("no WAL segments")
	}
	return matches
}
