package auditwal

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type segment struct {
	path     string
	first    Cursor
	last     Cursor
	oldestAt time.Time
	newestAt time.Time
	bytes    int64
}

type activeSegment struct {
	file    *os.File
	segment *segment
	pending int64
}

type WAL struct {
	mu            sync.Mutex
	dir           string
	agentID       string
	options       Options
	meta          metadata
	segments      []*segment
	active        *activeSegment
	totalBytes    int64
	closed        bool
	backgroundErr error
	stop          chan struct{}
	done          chan struct{}
}

// Recover scans and repairs only the last segment, then returns the actual WAL
// tail needed by the Agent clean-close startup sequence. Call it before
// incrementing the Agent incarnation; Open starts the new incarnation after.
func Recover(dir, agentID string, options Options) (Recovery, error) {
	options = normalizeOptions(options)
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return Recovery{}, nil
	} else if err != nil {
		return Recovery{}, err
	}
	if err := validateWALDirectory(dir); err != nil {
		return Recovery{}, err
	}
	meta, exists, err := readMetadata(dir)
	if err != nil {
		return Recovery{}, err
	}
	if exists && (meta.Version != metadataVersion || meta.AgentID != agentID) {
		return Recovery{}, fmt.Errorf("%w: metadata identity/version mismatch", ErrInvariant)
	}
	w := &WAL{dir: dir, agentID: agentID, options: options, meta: meta}
	if err := w.recoverSegments(); err != nil {
		return Recovery{}, err
	}
	if !exists && len(w.segments) != 0 {
		return Recovery{}, fmt.Errorf("%w: WAL segments exist without coverage metadata", ErrInvariant)
	}
	recovery := Recovery{}
	if len(w.segments) != 0 {
		recovery.WALTail = cloneCursor(&w.segments[len(w.segments)-1].last)
	}
	if exists {
		recovery.DurableThrough = cloneCursor(meta.DurableThrough)
	}
	return recovery, nil
}

func Open(dir, agentID string, incarnation uint64, options Options) (*WAL, error) {
	if agentID == "" || incarnation == 0 {
		return nil, fmt.Errorf("%w: agent_id and incarnation are required", ErrInvariant)
	}
	options = normalizeOptions(options)
	if err := ensureWALDirectory(dir); err != nil {
		return nil, err
	}
	meta, err := loadMetadata(dir, agentID, incarnation)
	if err != nil {
		return nil, err
	}
	w := &WAL{
		dir: dir, agentID: agentID, options: options, meta: meta,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	if err := w.recoverSegments(); err != nil {
		return nil, err
	}
	if err := w.reconcileRecoveredState(); err != nil {
		return nil, err
	}
	if _, err := w.enforceRetentionLocked(options.Now(), 0); err != nil {
		return nil, err
	}
	if err := saveMetadata(w.dir, w.meta); err != nil {
		return nil, fmt.Errorf("auditwal: persist recovered metadata: %w", err)
	}
	go w.syncLoop()
	return w, nil
}

func normalizeOptions(options Options) Options {
	defaults := DefaultOptions()
	if options.MaxBytes <= 0 {
		options.MaxBytes = defaults.MaxBytes
	}
	if options.MaxAge <= 0 {
		options.MaxAge = defaults.MaxAge
	}
	if options.SyncInterval <= 0 {
		options.SyncInterval = defaults.SyncInterval
	}
	if options.SyncBytes <= 0 {
		options.SyncBytes = defaults.SyncBytes
	}
	if options.Now == nil {
		options.Now = defaults.Now
	}
	return options
}

func ensureWALDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("auditwal: create directory: %w", err)
		}
		info, err = os.Lstat(dir)
	}
	if err != nil {
		return err
	}
	return validateWALDirectoryInfo(dir, info)
}

func validateWALDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	return validateWALDirectoryInfo(dir, info)
}

func validateWALDirectoryInfo(dir string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%w: WAL directory %q must be a non-symlink directory with mode 0700", ErrInvariant, dir)
	}
	return nil
}

func (w *WAL) Append(ctx context.Context, payload []byte) (Record, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.appendLocked(ctx, payload)
}

func (w *WAL) appendLocked(ctx context.Context, payload []byte) (Record, error) {
	if w.closed {
		return Record{}, ErrClosed
	}
	if w.backgroundErr != nil {
		return Record{}, w.backgroundErr
	}
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	cursor := Cursor{Incarnation: w.meta.CurrentIncarnation, Seq: w.meta.NextSeq}
	if cursor.Seq == math.MaxUint64 {
		return Record{}, fmt.Errorf("%w: sequence exhausted", ErrInvariant)
	}
	now := w.options.Now().UTC()
	if int64(len(payload)) > maxPayloadBytes(w.options.MaxBytes) {
		return Record{}, ErrRecordTooLarge
	}
	frame, err := encodeFrame(cursor, now, payload)
	if err != nil {
		return Record{}, err
	}
	if int64(len(frame)) > w.options.MaxBytes {
		return Record{}, ErrRecordTooLarge
	}
	if w.active != nil && w.totalBytes+int64(len(frame)) > w.options.MaxBytes {
		if err := w.syncLocked(); err != nil {
			return Record{}, err
		}
	}
	if _, err := w.enforceRetentionLocked(now, int64(len(frame))); err != nil {
		return Record{}, err
	}
	if w.active == nil {
		if err := w.createActiveLocked(cursor, now); err != nil {
			return Record{}, err
		}
	}
	if err := writeFull(w.active.file, frame); err != nil {
		return Record{}, fmt.Errorf("auditwal: append: %w", err)
	}
	w.active.pending += int64(len(frame))
	w.active.segment.bytes += int64(len(frame))
	w.active.segment.last = cursor
	w.active.segment.newestAt = now
	w.totalBytes += int64(len(frame))
	w.meta.NextSeq++
	w.meta.LastAssigned = cloneCursor(&cursor)
	record := Record{AgentID: w.agentID, Cursor: cursor, AppendedAt: now, Payload: append([]byte(nil), payload...)}
	if w.active.pending >= w.options.SyncBytes {
		if err := w.syncLocked(); err != nil {
			return record, err
		}
	}
	return record, nil
}

func (w *WAL) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	err := w.syncLocked()
	if err == nil {
		w.backgroundErr = nil
	}
	return err
}

func (w *WAL) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	err := w.syncLocked()
	if err == nil && w.backgroundErr != nil {
		err = w.backgroundErr
	}
	if w.active != nil {
		if closeErr := w.active.file.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
		w.active = nil
	}
	w.closed = true
	close(w.stop)
	w.mu.Unlock()
	<-w.done
	return err
}

func (w *WAL) syncLoop() {
	ticker := time.NewTicker(w.options.SyncInterval)
	defer func() { ticker.Stop(); close(w.done) }()
	for {
		select {
		case <-ticker.C:
			w.mu.Lock()
			if !w.closed {
				if w.active != nil && w.active.pending > 0 {
					if err := w.syncLocked(); err != nil {
						w.backgroundErr = err
					}
				}
				if w.backgroundErr == nil {
					if _, err := w.enforceRetentionLocked(w.options.Now(), 0); err != nil {
						w.backgroundErr = err
					}
				}
			}
			w.mu.Unlock()
		case <-w.stop:
			return
		}
	}
}

func (w *WAL) createActiveLocked(cursor Cursor, now time.Time) error {
	name := segmentName(cursor)
	path := filepath.Join(w.dir, name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("auditwal: create segment: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		os.Remove(path)
		return fmt.Errorf("auditwal: secure segment: %w", err)
	}
	seg := &segment{path: path, first: cursor, last: cursor, oldestAt: now, newestAt: now}
	w.segments = append(w.segments, seg)
	w.active = &activeSegment{file: file, segment: seg}
	return nil
}

func (w *WAL) syncLocked() error {
	if w.active == nil {
		return nil
	}
	if err := w.active.file.Sync(); err != nil {
		return fmt.Errorf("auditwal: fsync segment: %w", err)
	}
	previous := cloneCursor(w.meta.DurableThrough)
	w.meta.DurableThrough = cloneCursor(&w.active.segment.last)
	if err := saveMetadata(w.dir, w.meta); err != nil {
		w.meta.DurableThrough = previous
		return fmt.Errorf("auditwal: persist durable cursor: %w", err)
	}
	file := w.active.file
	w.active = nil
	if err := file.Close(); err != nil {
		return fmt.Errorf("auditwal: close segment: %w", err)
	}
	return nil
}

func (w *WAL) recoverSegments() error {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return err
	}
	var paths []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "segment-") && strings.HasSuffix(entry.Name(), ".wal") {
			if _, ok := parseSegmentName(entry.Name()); !ok {
				return fmt.Errorf("%w: invalid segment filename %q", ErrCorrupt, entry.Name())
			}
			paths = append(paths, filepath.Join(w.dir, entry.Name()))
		}
	}
	sort.Strings(paths)
	for index, path := range paths {
		seg, err := scanSegment(path, index == len(paths)-1, w.options.MaxBytes)
		if err != nil {
			return err
		}
		if seg == nil {
			continue
		}
		w.segments = append(w.segments, seg)
		w.totalBytes += seg.bytes
	}
	return w.validateSegmentContinuity()
}

func scanSegment(path string, repairTail bool, maxFrameBytes int64) (*segment, error) {
	flags := syscall.O_RDONLY
	if repairTail {
		flags = syscall.O_RDWR
	}
	file, err := openSecureWALFile(path, flags)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	fileInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	var seg *segment
	var offset int64
	for {
		frame, err := readFrame(file, maxFrameBytes)
		if err == nil {
			if seg == nil {
				seg = &segment{path: path, first: frame.cursor, oldestAt: frame.at}
			}
			if seg.last != (Cursor{}) && (frame.cursor.Incarnation != seg.last.Incarnation || frame.cursor.Seq != seg.last.Seq+1) {
				return nil, fmt.Errorf("%w: non-contiguous segment %s", ErrCorrupt, path)
			}
			seg.last, seg.newestAt = frame.cursor, frame.at
			offset += frame.bytes
			continue
		}
		if errors.Is(err, io.EOF) {
			break
		}
		position, seekErr := file.Seek(0, io.SeekCurrent)
		if seekErr != nil {
			return nil, seekErr
		}
		// An incomplete read is necessarily a tail failure. A checksum/length
		// failure is repairable only when the failed frame reaches physical EOF;
		// corruption followed by more frames must never be silently truncated.
		repairableCorruptTail := errors.Is(err, ErrCorrupt) &&
			(position == fileInfo.Size() || declaredFrameRunsPastEOF(file, offset, fileInfo.Size()))
		if repairTail && (errors.Is(err, io.ErrUnexpectedEOF) || repairableCorruptTail) {
			if err := file.Truncate(offset); err != nil {
				return nil, err
			}
			if err := file.Sync(); err != nil {
				return nil, err
			}
			break
		}
		return nil, fmt.Errorf("%w: scan %s at %d: %v", ErrCorrupt, path, offset, err)
	}
	if seg == nil {
		if !repairTail {
			return nil, fmt.Errorf("%w: empty non-tail segment %s", ErrCorrupt, path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			return nil, err
		}
		return nil, nil
	}
	seg.bytes = offset
	want, ok := parseSegmentName(filepath.Base(path))
	if !ok || want != seg.first {
		return nil, fmt.Errorf("%w: segment filename/start mismatch", ErrCorrupt)
	}
	return seg, nil
}

func declaredFrameRunsPastEOF(file *os.File, offset, size int64) bool {
	var length [4]byte
	if _, err := file.ReadAt(length[:], offset); err != nil {
		return true
	}
	declaredBytes := int64(4) + int64(binary.BigEndian.Uint32(length[:])) + int64(4)
	return size-offset < declaredBytes
}

func (w *WAL) reconcileRecoveredState() error {
	var last *Cursor
	for _, seg := range w.segments {
		if last != nil && compareCursor(seg.first, *last) <= 0 {
			return fmt.Errorf("%w: overlapping segments", ErrCorrupt)
		}
		last = cloneCursor(&seg.last)
	}
	if last != nil && (w.meta.LastAssigned == nil || compareCursor(*last, *w.meta.LastAssigned) > 0) {
		w.meta.LastAssigned = cloneCursor(last)
	}
	var currentLast uint64
	for _, seg := range w.segments {
		if seg.last.Incarnation == w.meta.CurrentIncarnation && seg.last.Seq > currentLast {
			currentLast = seg.last.Seq
		}
	}
	if currentLast >= w.meta.NextSeq {
		w.meta.NextSeq = currentLast + 1
	}
	return nil
}

func segmentName(cursor Cursor) string {
	return fmt.Sprintf("segment-%020d-%020d.wal", cursor.Incarnation, cursor.Seq)
}

func parseSegmentName(name string) (Cursor, bool) {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(name, "segment-"), ".wal")
	parts := strings.Split(trimmed, "-")
	if len(parts) != 2 {
		return Cursor{}, false
	}
	inc, err1 := strconv.ParseUint(parts[0], 10, 64)
	seq, err2 := strconv.ParseUint(parts[1], 10, 64)
	cursor := Cursor{inc, seq}
	return cursor, err1 == nil && err2 == nil && inc != 0 && seq != 0 && segmentName(cursor) == name
}

func openSecureWALFile(path string, flags int) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("%w: unsafe WAL file %q", ErrInvariant, filepath.Base(path))
	}
	fd, err := syscall.Open(path, flags|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Mode().Perm() != 0o600 {
		file.Close()
		return nil, fmt.Errorf("%w: WAL file changed while opening", ErrInvariant)
	}
	return file, nil
}

func maxPayloadBytes(maxFrameBytes int64) int64 {
	maximum := maxFrameBytes - envelopeHeaderBytes - 8
	uint32Maximum := int64(math.MaxUint32) - envelopeHeaderBytes
	if maximum > uint32Maximum {
		maximum = uint32Maximum
	}
	if maximum < 0 {
		return 0
	}
	return maximum
}

func (w *WAL) validateSegmentContinuity() error {
	for index, seg := range w.segments {
		if index == 0 || w.segments[index-1].last.Incarnation != seg.first.Incarnation {
			if seg.first.Seq > 1 && !w.rangeExplained(seg.first.Incarnation, 1, seg.first.Seq) {
				return fmt.Errorf("%w: unexplained missing prefix before %v", ErrInvariant, seg.first)
			}
			continue
		}
		previous := w.segments[index-1]
		if previous.last.Seq == math.MaxUint64 || seg.first.Seq <= previous.last.Seq ||
			(seg.first.Seq != previous.last.Seq+1 && !w.rangeExplained(seg.first.Incarnation, previous.last.Seq+1, seg.first.Seq)) {
			return fmt.Errorf("%w: unexplained segment discontinuity between %v and %v", ErrInvariant, previous.last, seg.first)
		}
	}
	return nil
}

func (w *WAL) rangeExplained(incarnation, from, until uint64) bool {
	if from >= until {
		return true
	}
	for _, unknown := range w.meta.CoverageUnknownIncarnations {
		if unknown == incarnation {
			return true
		}
	}
	if ack := w.meta.ServerACK; ack != nil {
		if ack.Incarnation > incarnation {
			return true
		}
		if ack.Incarnation == incarnation && ack.Seq >= from {
			if ack.Seq == math.MaxUint64 || ack.Seq+1 >= until {
				return true
			}
			from = ack.Seq + 1
		}
	}
	for _, gap := range w.meta.Gaps {
		if gap.Incarnation != incarnation || gap.UntilSeq <= from || gap.FromSeq > from {
			continue
		}
		from = gap.UntilSeq
		if from >= until {
			return true
		}
	}
	return false
}
