package auditwal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	onceReceiptVersion = 1
	onceKeyMaxBytes    = 512
)

type onceReceipt struct {
	Version       int       `json:"version"`
	Key           string    `json:"key"`
	PayloadSHA256 string    `json:"payload_sha256"`
	Cursor        *Cursor   `json:"cursor,omitempty"`
	AppendedAt    time.Time `json:"appended_at,omitempty"`
}

// AppendOnce appends payload at most once while its durable key receipt is
// retained. It is intended for a durable outbox: reserve the key, fsync the
// WAL record, persist its cursor, then let the outbox mark delivery before it
// calls ForgetOnce. A crash between record fsync and receipt completion is
// recovered by finding the exact payload already present in the WAL.
func (w *WAL) AppendOnce(ctx context.Context, key string, payload []byte) (Record, error) {
	if !validOnceKey(key) {
		return Record{}, fmt.Errorf("%w: invalid idempotency key", ErrInvariant)
	}
	digest := sha256.Sum256(payload)
	payloadDigest := hex.EncodeToString(digest[:])

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return Record{}, ErrClosed
	}
	if w.backgroundErr != nil {
		return Record{}, w.backgroundErr
	}
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}

	receipt, exists, err := w.loadOnceReceiptLocked(key)
	if err != nil {
		return Record{}, err
	}
	if exists {
		if receipt.PayloadSHA256 != payloadDigest {
			return Record{}, fmt.Errorf("%w: idempotency key reused with different payload", ErrInvariant)
		}
		if receipt.Cursor != nil {
			return Record{AgentID: w.agentID, Cursor: *receipt.Cursor, AppendedAt: receipt.AppendedAt, Payload: append([]byte(nil), payload...)}, nil
		}
		if recovered, ok, findErr := w.findPayloadLocked(payload); findErr != nil {
			return Record{}, findErr
		} else if ok {
			receipt.Cursor = cloneCursor(&recovered.Cursor)
			receipt.AppendedAt = recovered.AppendedAt
			if err := w.saveOnceReceiptLocked(receipt); err != nil {
				return recovered, err
			}
			return recovered, nil
		}
	} else {
		receipt = onceReceipt{Version: onceReceiptVersion, Key: key, PayloadSHA256: payloadDigest}
		if err := w.saveOnceReceiptLocked(receipt); err != nil {
			return Record{}, err
		}
	}

	record, err := w.appendLocked(ctx, payload)
	if err != nil {
		return record, err
	}
	// The receipt must never claim a cursor whose frame is still only in the
	// page cache. Managed operations are infrequent, so this stronger boundary
	// is preferable to weakening exactly-once replay semantics.
	if err := w.syncLocked(); err != nil {
		return record, err
	}
	receipt.Cursor = cloneCursor(&record.Cursor)
	receipt.AppendedAt = record.AppendedAt
	if err := w.saveOnceReceiptLocked(receipt); err != nil {
		return record, err
	}
	return record, nil
}

// ForgetOnce removes a receipt only after the source outbox durably records
// delivery. Removing an absent receipt is idempotent.
func (w *WAL) ForgetOnce(key string) error {
	if !validOnceKey(key) {
		return fmt.Errorf("%w: invalid idempotency key", ErrInvariant)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	path := filepath.Join(w.dir, onceReceiptName(key))
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("auditwal: remove idempotency receipt: %w", err)
	}
	return syncDirectory(w.dir)
}

func validOnceKey(key string) bool {
	return key != "" && len(key) <= onceKeyMaxBytes && utf8.ValidString(key) && !strings.ContainsAny(key, "\x00\r\n")
}

func onceReceiptName(key string) string {
	digest := sha256.Sum256([]byte(key))
	return "once-" + hex.EncodeToString(digest[:]) + ".json"
}

func (w *WAL) loadOnceReceiptLocked(key string) (onceReceipt, bool, error) {
	path := filepath.Join(w.dir, onceReceiptName(key))
	file, err := openSecureWALFile(path, syscall.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return onceReceipt{}, false, nil
	}
	if err != nil {
		return onceReceipt{}, false, fmt.Errorf("auditwal: read idempotency receipt: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 4096))
	decoder.DisallowUnknownFields()
	var receipt onceReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return onceReceipt{}, false, fmt.Errorf("%w: invalid idempotency receipt", ErrInvariant)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) || validateOnceReceipt(receipt, key) != nil {
		return onceReceipt{}, false, fmt.Errorf("%w: invalid idempotency receipt", ErrInvariant)
	}
	return receipt, true, nil
}

func validateOnceReceipt(receipt onceReceipt, key string) error {
	if receipt.Version != onceReceiptVersion || receipt.Key != key || !validOnceKey(receipt.Key) || len(receipt.PayloadSHA256) != 64 {
		return ErrInvariant
	}
	if _, err := hex.DecodeString(receipt.PayloadSHA256); err != nil || receipt.PayloadSHA256 != strings.ToLower(receipt.PayloadSHA256) {
		return ErrInvariant
	}
	if (receipt.Cursor == nil) != receipt.AppendedAt.IsZero() {
		return ErrInvariant
	}
	if receipt.Cursor != nil && (receipt.Cursor.Incarnation == 0 || receipt.Cursor.Seq == 0) {
		return ErrInvariant
	}
	return nil
}

func (w *WAL) saveOnceReceiptLocked(receipt onceReceipt) error {
	if err := validateOnceReceipt(receipt, receipt.Key); err != nil {
		return err
	}
	payload, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	temporary, err := os.CreateTemp(w.dir, ".once-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if err := writeFull(temporary, payload); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, filepath.Join(w.dir, onceReceiptName(receipt.Key))); err != nil {
		return err
	}
	return syncDirectory(w.dir)
}

func (w *WAL) findPayloadLocked(payload []byte) (Record, bool, error) {
	for _, segment := range w.segments {
		file, err := openSecureWALFile(segment.path, syscall.O_RDONLY)
		if err != nil {
			return Record{}, false, err
		}
		for {
			frame, readErr := readFrame(file, w.options.MaxBytes)
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				file.Close()
				return Record{}, false, readErr
			}
			if bytes.Equal(frame.payload, payload) {
				file.Close()
				return Record{AgentID: w.agentID, Cursor: frame.cursor, AppendedAt: frame.at, Payload: append([]byte(nil), payload...)}, true, nil
			}
		}
		if err := file.Close(); err != nil {
			return Record{}, false, err
		}
	}
	return Record{}, false, nil
}
