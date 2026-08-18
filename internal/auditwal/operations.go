package auditwal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

func (w *WAL) ReadAuditFrom(ctx context.Context, start Cursor, limit int) (ReadResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ReadResult{}, ErrClosed
	}
	if start.Incarnation == 0 || start.Seq == 0 || limit <= 0 {
		return ReadResult{}, fmt.Errorf("%w: valid start cursor and positive limit required", ErrInvariant)
	}
	bounds := w.boundsLocked()
	coverage := w.coverageLocked(w.options.Now())
	if bounds.WALFloor != nil && compareCursor(start, *bounds.WALFloor) < 0 {
		return ReadResult{BehindFloor: &CursorBehindFloor{
			Requested: start, Bounds: bounds, Coverage: coverage,
		}}, nil
	}
	result := ReadResult{Records: make([]Record, 0, limit)}
	for _, seg := range w.segments {
		if compareCursor(seg.last, start) < 0 {
			continue
		}
		file, err := openSecureWALFile(seg.path, syscall.O_RDONLY)
		if err != nil {
			return ReadResult{}, err
		}
		expected := seg.first
		lastRead := Cursor{}
		for len(result.Records) < limit {
			if err := ctx.Err(); err != nil {
				file.Close()
				return ReadResult{}, err
			}
			frame, err := readFrame(file, w.options.MaxBytes)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				file.Close()
				return ReadResult{}, fmt.Errorf("%w: read segment: %v", ErrCorrupt, err)
			}
			if frame.cursor != expected {
				file.Close()
				return ReadResult{}, fmt.Errorf("%w: segment cursor changed after recovery", ErrCorrupt)
			}
			lastRead = frame.cursor
			if expected.Seq != ^uint64(0) {
				expected.Seq++
			}
			if compareCursor(frame.cursor, start) >= 0 {
				result.Records = append(result.Records, Record{
					AgentID: w.agentID, Cursor: frame.cursor, AppendedAt: frame.at,
					Payload: frame.payload,
				})
			}
		}
		if closeErr := file.Close(); closeErr != nil {
			return ReadResult{}, closeErr
		}
		if len(result.Records) < limit && lastRead != seg.last {
			return ReadResult{}, fmt.Errorf("%w: segment boundary changed after recovery", ErrCorrupt)
		}
		if len(result.Records) == limit {
			break
		}
	}
	return result, nil
}

func (w *WAL) Bounds() (Bounds, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return Bounds{}, ErrClosed
	}
	return w.boundsLocked(), nil
}

func (w *WAL) boundsLocked() Bounds {
	bounds := Bounds{
		NextCursor:            Cursor{Incarnation: w.meta.CurrentIncarnation, Seq: w.meta.NextSeq},
		DurableThrough:        cloneCursor(w.meta.DurableThrough),
		ServerACKedThrough:    cloneCursor(w.meta.ServerACK),
		AcknowledgedArchiveID: w.meta.AcknowledgedArchive,
		CoverageRevision:      w.meta.CoverageRevision,
	}
	if len(w.segments) > 0 {
		bounds.WALFloor = cloneCursor(&w.segments[0].first)
		bounds.WALCeiling = cloneCursor(&w.segments[len(w.segments)-1].last)
	}
	return bounds
}

func (w *WAL) GetAuditCoverage() (CoverageSnapshot, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return CoverageSnapshot{}, ErrClosed
	}
	return w.coverageLocked(w.options.Now()), nil
}

func (w *WAL) coverageLocked(now time.Time) CoverageSnapshot {
	return CoverageSnapshot{
		AgentID: w.agentID, Revision: w.meta.CoverageRevision,
		GeneratedAt: now.UTC(), Gaps: cloneGaps(w.meta.Gaps),
		CoverageUnknownIncarnations: append([]uint64(nil), w.meta.CoverageUnknownIncarnations...),
	}
}

// RebindArchive is called only after the Agent archive-generation state
// machine has accepted a forward rebind. It resets the retired archive ACK.
func (w *WAL) RebindArchive(auditArchiveID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	if auditArchiveID == "" {
		return fmt.Errorf("%w: empty archive id", ErrInvariant)
	}
	if w.meta.AcknowledgedArchive == auditArchiveID {
		return nil
	}
	if err := w.syncLocked(); err != nil {
		return err
	}
	oldArchive, oldACK := w.meta.AcknowledgedArchive, cloneCursor(w.meta.ServerACK)
	w.meta.AcknowledgedArchive = auditArchiveID
	w.meta.ServerACK = nil
	if err := saveMetadata(w.dir, w.meta); err != nil {
		w.meta.AcknowledgedArchive, w.meta.ServerACK = oldArchive, oldACK
		return err
	}
	return nil
}

func (w *WAL) AckAudit(auditArchiveID string, proposed Cursor, coverageRevisionSeen uint64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	if auditArchiveID == "" || proposed.Incarnation == 0 || proposed.Seq == 0 {
		return fmt.Errorf("%w: archive and cursor required", ErrInvariant)
	}
	if w.meta.AcknowledgedArchive != auditArchiveID {
		return fmt.Errorf("%w: active %q, proposed %q", ErrArchiveMismatch, w.meta.AcknowledgedArchive, auditArchiveID)
	}
	// ACK persistence can carry next_seq. Flush the current batch first so a
	// crash cannot leave a persisted sequence hole without a record or GAP.
	if err := w.syncLocked(); err != nil {
		return err
	}
	if w.meta.ServerACK != nil {
		comparison := compareCursor(proposed, *w.meta.ServerACK)
		if comparison < 0 {
			return ErrCursorRollback
		}
		if comparison == 0 {
			_, err := w.deleteACKedLocked()
			return err
		}
	}
	if coverageRevisionSeen > w.meta.CoverageRevision {
		return fmt.Errorf("%w: seen coverage revision %d exceeds current %d",
			ErrInvariant, coverageRevisionSeen, w.meta.CoverageRevision)
	}
	if w.meta.LastAssigned == nil || compareCursor(proposed, *w.meta.LastAssigned) > 0 {
		return ErrCursorAhead
	}
	blocking := make([]Gap, 0)
	for _, gap := range w.meta.Gaps {
		if gap.LastLossRevision > coverageRevisionSeen && gapIntersectsACK(gap, w.meta.ServerACK, proposed) {
			blocking = append(blocking, gap)
		}
	}
	blockingUnknown := make([]uint64, 0)
	if coverageRevisionSeen < w.meta.CoverageRevision {
		for _, incarnation := range w.meta.CoverageUnknownIncarnations {
			if incarnationIntersectsACK(incarnation, w.meta.ServerACK, proposed) {
				blockingUnknown = append(blockingUnknown, incarnation)
			}
		}
	}
	if len(blocking) > 0 || len(blockingUnknown) > 0 {
		return &StaleCoverageError{
			SeenRevision: coverageRevisionSeen, CurrentRevision: w.meta.CoverageRevision,
			CurrentACK: cloneCursor(w.meta.ServerACK), BlockingRanges: cloneGaps(blocking),
			BlockingUnknownIncarnations: append([]uint64(nil), blockingUnknown...), Coverage: w.coverageLocked(w.options.Now()),
		}
	}
	old := cloneCursor(w.meta.ServerACK)
	w.meta.ServerACK = cloneCursor(&proposed)
	if err := saveMetadata(w.dir, w.meta); err != nil {
		w.meta.ServerACK = old
		return fmt.Errorf("auditwal: persist ACK: %w", err)
	}
	_, err := w.deleteACKedLocked()
	return err
}

// EnforceLimits applies the first-hit age/size retention policy. Any un-ACKed
// deletion is recorded durably as an exact RETENTION gap before unlink.
func (w *WAL) EnforceLimits(now time.Time) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, ErrClosed
	}
	if err := w.syncLocked(); err != nil {
		return 0, err
	}
	return w.enforceRetentionLocked(now, 0)
}

func (w *WAL) enforceRetentionLocked(now time.Time, incoming int64) (int64, error) {
	var freed int64
	acked, err := w.deleteACKedLocked()
	if err != nil {
		return 0, err
	}
	freed += acked
	cutoff := now.UTC().Add(-w.options.MaxAge)
	for len(w.segments) > 0 {
		seg := w.segments[0]
		if w.active != nil && seg == w.active.segment {
			break
		}
		overAge := !seg.newestAt.After(cutoff)
		overSize := w.totalBytes+incoming > w.options.MaxBytes
		if !overAge && !overSize {
			break
		}
		if err := w.deleteWithGapLocked(seg, GapRetention); err != nil {
			return freed, err
		}
		freed += seg.bytes
	}
	return freed, nil
}

// ReclaimACKedForDiskPressure removes only Server-ACKed closed segments. It is
// the second global eviction tier and deliberately cannot cross into un-ACKed
// data; the caller must run Operation and snapshot tiers before invoking the
// separate un-ACKed method.
func (w *WAL) ReclaimACKedForDiskPressure(bytesNeeded int64) (ReclaimResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ReclaimResult{}, ErrClosed
	}
	if bytesNeeded <= 0 {
		return ReclaimResult{Coverage: w.coverageLocked(w.options.Now())}, nil
	}
	result := ReclaimResult{}
	for len(w.segments) > 0 && result.FreedBytes < bytesNeeded {
		seg := w.segments[0]
		if w.active != nil && seg == w.active.segment {
			break
		}
		if !segmentACKed(seg, w.meta.ServerACK) {
			break
		}
		if err := w.deleteSegmentLocked(seg); err != nil {
			return result, err
		}
		result.FreedBytes += seg.bytes
		result.DeletedACKedBytes += seg.bytes
	}
	result.Degraded = result.FreedBytes < bytesNeeded
	result.Coverage = w.coverageLocked(w.options.Now())
	return result, nil
}

// ReclaimUnackedForDiskPressure removes only un-ACKed closed segments and
// records exact DISK_PRESSURE gaps before unlink. It is the final destructive
// tier; the active write batch is never removed.
func (w *WAL) ReclaimUnackedForDiskPressure(bytesNeeded int64) (ReclaimResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ReclaimResult{}, ErrClosed
	}
	if bytesNeeded <= 0 {
		return ReclaimResult{Coverage: w.coverageLocked(w.options.Now())}, nil
	}
	result := ReclaimResult{}
	for len(w.segments) > 0 && result.FreedBytes < bytesNeeded {
		seg := w.segments[0]
		if w.active != nil && seg == w.active.segment {
			break
		}
		if segmentACKed(seg, w.meta.ServerACK) {
			// The global coordinator must exhaust the ACKed tier first. Do not
			// silently skip it and thereby obscure an ordering bug.
			break
		}
		if err := w.deleteWithGapLocked(seg, GapDiskPressure); err != nil {
			return result, err
		}
		result.FreedBytes += seg.bytes
		result.DeletedUnackedBytes += seg.bytes
	}
	result.Degraded = result.FreedBytes < bytesNeeded
	result.Coverage = w.coverageLocked(w.options.Now())
	return result, nil
}

// ReclaimAbandonedTempForDiskPressure removes only interrupted metadata and
// idempotency-receipt temporary files. Holding w.mu excludes a concurrent
// writer, and unknown entries or symlinks are never selected.
func (w *WAL) ReclaimAbandonedTempForDiskPressure(ctx context.Context, bytesNeeded int64) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, ErrClosed
	}
	if bytesNeeded <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return 0, err
	}
	var freed int64
	for _, entry := range entries {
		if freed >= bytesNeeded {
			break
		}
		if err := ctx.Err(); err != nil {
			return freed, err
		}
		name := entry.Name()
		eligible := strings.HasSuffix(name, ".tmp") &&
			(strings.HasPrefix(name, ".coverage-state-") || strings.HasPrefix(name, ".once-"))
		if !eligible {
			continue
		}
		path := filepath.Join(w.dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return freed, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return freed, fmt.Errorf("%w: unsafe abandoned temporary file %q", ErrInvariant, name)
		}
		if err := os.Remove(path); err != nil {
			return freed, err
		}
		freed += info.Size()
	}
	if freed > 0 {
		if err := syncDirectory(w.dir); err != nil {
			return freed, err
		}
	}
	return freed, nil
}

// ReclaimForDiskPressure is retained for callers that have already exhausted
// every higher-priority global tier. New global coordination should call the
// split ACKed and un-ACKed methods at their respective positions.
func (w *WAL) ReclaimForDiskPressure(bytesNeeded int64) (ReclaimResult, error) {
	acked, err := w.ReclaimACKedForDiskPressure(bytesNeeded)
	if err != nil || acked.FreedBytes >= bytesNeeded {
		return acked, err
	}
	unacked, err := w.ReclaimUnackedForDiskPressure(bytesNeeded - acked.FreedBytes)
	acked.FreedBytes += unacked.FreedBytes
	acked.DeletedUnackedBytes += unacked.DeletedUnackedBytes
	acked.Degraded = acked.FreedBytes < bytesNeeded
	acked.Coverage = unacked.Coverage
	return acked, err
}

func (w *WAL) deleteACKedLocked() (int64, error) {
	var freed int64
	for len(w.segments) > 0 {
		seg := w.segments[0]
		if w.active != nil && seg == w.active.segment {
			break
		}
		if !segmentACKed(seg, w.meta.ServerACK) {
			break
		}
		if err := w.deleteSegmentLocked(seg); err != nil {
			return freed, err
		}
		freed += seg.bytes
	}
	return freed, nil
}

func (w *WAL) deleteWithGapLocked(seg *segment, reason GapReason) error {
	if seg.last.Seq == ^uint64(0) {
		return fmt.Errorf("%w: cannot represent gap end", ErrInvariant)
	}
	oldRevision := w.meta.CoverageRevision
	oldGaps := cloneGaps(w.meta.Gaps)
	w.meta.CoverageRevision++
	gap := Gap{
		Incarnation: seg.first.Incarnation, FromSeq: seg.first.Seq,
		UntilSeq: seg.last.Seq + 1, Reason: reason, Precision: PrecisionExact,
		LastLossRevision: w.meta.CoverageRevision,
	}
	w.meta.Gaps = mergeGap(w.meta.Gaps, gap)
	if err := w.saveMetadataDurableLocked(); err != nil {
		w.meta.CoverageRevision = oldRevision
		w.meta.Gaps = oldGaps
		return fmt.Errorf("auditwal: persist coverage before deletion: %w", err)
	}
	return w.deleteSegmentLocked(seg)
}

func (w *WAL) deleteSegmentLocked(seg *segment) error {
	if len(w.segments) == 0 || w.segments[0] != seg {
		return fmt.Errorf("%w: deletion is not oldest-first", ErrInvariant)
	}
	if err := os.Remove(seg.path); err != nil {
		return err
	}
	w.segments = w.segments[1:]
	w.totalBytes -= seg.bytes
	// Once unlink succeeds the in-memory index must stop advertising the file,
	// even if the following directory fsync reports an uncertain durability
	// outcome. The caller still receives that error; retaining a nonexistent
	// segment would make every retry fail forever.
	return syncDirectory(w.dir)
}

// saveMetadataDurableLocked avoids publishing an unsynced active batch's
// next_seq/last_assigned while still allowing out-of-band coverage to be
// persisted before deleting older segments.
func (w *WAL) saveMetadataDurableLocked() error {
	persisted := w.meta
	if w.active != nil && w.active.pending > 0 {
		persisted.NextSeq = w.active.segment.first.Seq
		persisted.LastAssigned = cloneCursor(w.meta.DurableThrough)
		for index, segment := range w.segments {
			if segment == w.active.segment && index > 0 &&
				(persisted.LastAssigned == nil || compareCursor(w.segments[index-1].last, *persisted.LastAssigned) > 0) {
				persisted.LastAssigned = cloneCursor(&w.segments[index-1].last)
				break
			}
		}
	}
	return saveMetadata(w.dir, persisted)
}

func segmentACKed(seg *segment, ack *Cursor) bool {
	return ack != nil && compareCursor(seg.last, *ack) <= 0
}

func gapIntersectsACK(gap Gap, current *Cursor, proposed Cursor) bool {
	gapFirst := Cursor{gap.Incarnation, gap.FromSeq}
	gapLast := Cursor{gap.Incarnation, gap.UntilSeq - 1}
	if compareCursor(gapFirst, proposed) > 0 {
		return false
	}
	return current == nil || compareCursor(gapLast, *current) > 0
}

func incarnationIntersectsACK(incarnation uint64, current *Cursor, proposed Cursor) bool {
	if proposed.Incarnation < incarnation {
		return false
	}
	return current == nil || current.Incarnation <= incarnation
}

func mergeGap(gaps []Gap, incoming Gap) []Gap {
	result := append(cloneGaps(gaps), incoming)
	sort.Slice(result, func(i, j int) bool {
		if result[i].Incarnation != result[j].Incarnation {
			return result[i].Incarnation < result[j].Incarnation
		}
		return result[i].FromSeq < result[j].FromSeq
	})
	merged := make([]Gap, 0, len(result))
	for _, gap := range result {
		last := len(merged) - 1
		if last >= 0 && merged[last].Incarnation == gap.Incarnation &&
			merged[last].Reason == gap.Reason && merged[last].Precision == gap.Precision &&
			gap.FromSeq <= merged[last].UntilSeq {
			if gap.UntilSeq > merged[last].UntilSeq {
				merged[last].UntilSeq = gap.UntilSeq
			}
			if gap.LastLossRevision > merged[last].LastLossRevision {
				merged[last].LastLossRevision = gap.LastLossRevision
			}
			continue
		}
		merged = append(merged, gap)
	}
	return merged
}

func syncDirectory(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func compareCursor(left, right Cursor) int {
	if left.Incarnation < right.Incarnation {
		return -1
	}
	if left.Incarnation > right.Incarnation {
		return 1
	}
	if left.Seq < right.Seq {
		return -1
	}
	if left.Seq > right.Seq {
		return 1
	}
	return 0
}

func cloneCursor(cursor *Cursor) *Cursor {
	if cursor == nil {
		return nil
	}
	copy := *cursor
	return &copy
}

func cloneGaps(gaps []Gap) []Gap { return append([]Gap(nil), gaps...) }
