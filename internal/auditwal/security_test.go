package auditwal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWALRejectsSymlinkAndInsecureDirectories(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		base := t.TempDir()
		target := filepath.Join(base, "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "wal-link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(link, "agent-1", 1, testOptions()); !errors.Is(err, ErrInvariant) {
			t.Fatalf("Open symlink error=%v", err)
		}
		if _, err := Recover(link, "agent-1", testOptions()); !errors.Is(err, ErrInvariant) {
			t.Fatalf("Recover symlink error=%v", err)
		}
	})

	t.Run("insecure mode", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "wal")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, "agent-1", 1, testOptions()); !errors.Is(err, ErrInvariant) {
			t.Fatalf("Open insecure directory error=%v", err)
		}
	})
}

func TestWALRejectsUnsafeSegmentAndMetadataFiles(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string, string)
	}{
		{
			name: "segment symlink",
			mutate: func(t *testing.T, dir, segment string) {
				if err := os.Remove(segment); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(dir, metadataFile), segment); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "segment insecure mode",
			mutate: func(t *testing.T, _, segment string) {
				if err := os.Chmod(segment, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "metadata symlink",
			mutate: func(t *testing.T, dir, _ string) {
				metadataPath := filepath.Join(dir, metadataFile)
				backupPath := filepath.Join(dir, "metadata-backup")
				if err := os.Rename(metadataPath, backupPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(backupPath, metadataPath); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "metadata insecure mode",
			mutate: func(t *testing.T, dir, _ string) {
				if err := os.Chmod(filepath.Join(dir, metadataFile), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			wal := openTestWAL(t, dir, 1, testOptions())
			if _, err := wal.Append(context.Background(), []byte("record")); err != nil {
				t.Fatal(err)
			}
			if err := wal.Close(); err != nil {
				t.Fatal(err)
			}
			segment := segmentPaths(t, dir)[0]
			test.mutate(t, dir, segment)
			if _, err := Open(dir, "agent-1", 2, testOptions()); !errors.Is(err, ErrInvariant) {
				t.Fatalf("Open error=%v", err)
			}
		})
	}
}

func TestSegmentFilenameAndCursorDiscontinuitiesRequireCoverage(t *testing.T) {
	t.Run("noncanonical filename", func(t *testing.T) {
		dir := t.TempDir()
		wal := openTestWAL(t, dir, 1, testOptions())
		if _, err := wal.Append(context.Background(), []byte("record")); err != nil {
			t.Fatal(err)
		}
		if err := wal.Close(); err != nil {
			t.Fatal(err)
		}
		path := segmentPaths(t, dir)[0]
		if err := os.Rename(path, filepath.Join(dir, "segment-1-1.wal")); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, "agent-1", 2, testOptions()); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Open error=%v", err)
		}
	})

	t.Run("middle segment missing without gap", func(t *testing.T) {
		dir := makeThreeSegmentWAL(t)
		paths := segmentPaths(t, dir)
		if err := os.Remove(paths[1]); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, "agent-1", 2, testOptions()); !errors.Is(err, ErrInvariant) {
			t.Fatalf("Open error=%v", err)
		}
	})

	t.Run("first segment missing without ACK or gap", func(t *testing.T) {
		dir := makeThreeSegmentWAL(t)
		paths := segmentPaths(t, dir)
		if err := os.Remove(paths[0]); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, "agent-1", 2, testOptions()); !errors.Is(err, ErrInvariant) {
			t.Fatalf("Open error=%v", err)
		}
	})

	t.Run("explicit gap explains missing segment", func(t *testing.T) {
		dir := makeThreeSegmentWAL(t)
		paths := segmentPaths(t, dir)
		meta, exists, err := readMetadata(dir)
		if err != nil || !exists {
			t.Fatalf("metadata exists=%v err=%v", exists, err)
		}
		meta.CoverageRevision = 1
		meta.Gaps = []Gap{{Incarnation: 1, FromSeq: 2, UntilSeq: 3, Reason: GapDiskPressure, Precision: PrecisionExact, LastLossRevision: 1}}
		if err := saveMetadata(dir, meta); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(paths[1]); err != nil {
			t.Fatal(err)
		}
		wal, err := Open(dir, "agent-1", 2, testOptions())
		if err != nil {
			t.Fatal(err)
		}
		defer wal.Close()
		read, err := wal.ReadAuditFrom(context.Background(), Cursor{1, 1}, 10)
		if err != nil || len(read.Records) != 2 || read.Records[0].Cursor.Seq != 1 || read.Records[1].Cursor.Seq != 3 {
			t.Fatalf("read=%+v err=%v", read, err)
		}
	})
}

func TestReadRejectsSegmentReplacementAfterRecovery(t *testing.T) {
	dir := t.TempDir()
	options := testOptions()
	options.SyncBytes = 1
	wal := openTestWAL(t, dir, 1, options)
	if _, err := wal.Append(context.Background(), []byte("original")); err != nil {
		t.Fatal(err)
	}
	path := segmentPaths(t, dir)[0]
	replacement := filepath.Join(dir, "replacement")
	frame, err := encodeFrame(Cursor{1, 2}, options.Now(), []byte("replacement"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement, frame, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if _, err := wal.ReadAuditFrom(context.Background(), Cursor{1, 1}, 10); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ReadAuditFrom error=%v", err)
	}
}

func TestCoverageMetadataStrictValidation(t *testing.T) {
	base := metadata{
		Version: metadataVersion, AgentID: "agent", CurrentIncarnation: 2, NextSeq: 1,
		LastAssigned: &Cursor{1, 3}, DurableThrough: &Cursor{1, 3},
	}
	validGap := Gap{Incarnation: 1, FromSeq: 1, UntilSeq: 2, Reason: GapRetention, Precision: PrecisionExact, LastLossRevision: 1}
	tests := []struct {
		name   string
		mutate func(*metadata)
	}{
		{"zero incarnation", func(meta *metadata) { meta.CurrentIncarnation = 0 }},
		{"next sequence mismatch", func(meta *metadata) { meta.CurrentIncarnation = 1; meta.NextSeq = 9 }},
		{"durable ahead", func(meta *metadata) { meta.DurableThrough = &Cursor{2, 1} }},
		{"ACK without archive", func(meta *metadata) { meta.ServerACK = &Cursor{1, 1} }},
		{"gap at revision zero", func(meta *metadata) { meta.Gaps = []Gap{validGap} }},
		{"invalid gap range", func(meta *metadata) {
			meta.CoverageRevision = 1
			gap := validGap
			gap.UntilSeq = gap.FromSeq
			meta.Gaps = []Gap{gap}
		}},
		{"invalid gap reason", func(meta *metadata) {
			meta.CoverageRevision = 1
			gap := validGap
			gap.Reason = "invented"
			meta.Gaps = []Gap{gap}
		}},
		{"future gap revision", func(meta *metadata) {
			meta.CoverageRevision = 1
			gap := validGap
			gap.LastLossRevision = 2
			meta.Gaps = []Gap{gap}
		}},
		{"overlapping gaps", func(meta *metadata) { meta.CoverageRevision = 1; meta.Gaps = []Gap{validGap, validGap} }},
		{"unmerged adjacent gaps", func(meta *metadata) {
			meta.CoverageRevision = 1
			second := validGap
			second.FromSeq, second.UntilSeq = validGap.UntilSeq, validGap.UntilSeq+1
			meta.Gaps = []Gap{validGap, second}
		}},
		{"duplicate unknown", func(meta *metadata) { meta.CoverageRevision = 1; meta.CoverageUnknownIncarnations = []uint64{1, 1} }},
		{"unsorted unknown", func(meta *metadata) { meta.CoverageRevision = 1; meta.CoverageUnknownIncarnations = []uint64{2, 1} }},
		{"unknown and gap overlap", func(meta *metadata) {
			meta.CoverageRevision = 1
			meta.Gaps = []Gap{validGap}
			meta.CoverageUnknownIncarnations = []uint64{1}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			test.mutate(&candidate)
			if err := validateMetadata(candidate); !errors.Is(err, ErrInvariant) {
				t.Fatalf("validateMetadata error=%v candidate=%+v", err, candidate)
			}
		})
	}
	if err := validateMetadata(base); err != nil {
		t.Fatalf("valid metadata rejected: %v", err)
	}
}

func TestCoverageUnknownIncarnationBlocksStaleACK(t *testing.T) {
	dir := t.TempDir()
	options := testOptions()
	options.SyncBytes = 1
	wal := openTestWAL(t, dir, 1, options)
	if _, err := wal.Append(context.Background(), []byte("record")); err != nil {
		t.Fatal(err)
	}
	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}
	meta, exists, err := readMetadata(dir)
	if err != nil || !exists {
		t.Fatalf("metadata exists=%v err=%v", exists, err)
	}
	meta.CoverageRevision = 1
	meta.CoverageUnknownIncarnations = []uint64{1}
	if err := saveMetadata(dir, meta); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, "agent-1", 2, options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.RebindArchive("archive"); err != nil {
		t.Fatal(err)
	}
	err = reopened.AckAudit("archive", Cursor{1, 1}, 0)
	var stale *StaleCoverageError
	if !errors.As(err, &stale) || len(stale.BlockingUnknownIncarnations) != 1 || stale.BlockingUnknownIncarnations[0] != 1 {
		t.Fatalf("stale error=%#v err=%v", stale, err)
	}
	if err := reopened.AckAudit("archive", Cursor{1, 1}, 1); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataReaderRejectsUnknownAndTrailingFields(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, metadataFile)
	for _, payload := range []string{
		`{"version":1,"agent_id":"agent","current_incarnation":1,"next_seq":1,"coverage_revision":0,"unknown":true}`,
		`{"version":1,"agent_id":"agent","current_incarnation":1,"next_seq":1,"coverage_revision":0} {}`,
	} {
		if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readMetadata(dir); !errors.Is(err, ErrInvariant) {
			t.Fatalf("readMetadata error=%v payload=%s", err, payload)
		}
	}
}

func TestRecordPayloadLimitMatchesFrameReadContract(t *testing.T) {
	options := testOptions()
	options.MaxBytes = 64
	maximum := maxPayloadBytes(options.MaxBytes)
	if maximum != 32 {
		t.Fatalf("maximum payload=%d", maximum)
	}
	dir := t.TempDir()
	wal := openTestWAL(t, dir, 1, options)
	payload := make([]byte, maximum)
	if _, err := wal.Append(context.Background(), payload); err != nil {
		t.Fatalf("boundary append: %v", err)
	}
	if err := wal.Sync(); err != nil {
		t.Fatal(err)
	}
	read, err := wal.ReadAuditFrom(context.Background(), Cursor{1, 1}, 1)
	if err != nil || len(read.Records) != 1 || len(read.Records[0].Payload) != int(maximum) {
		t.Fatalf("boundary read=%+v err=%v", read, err)
	}

	other := t.TempDir()
	oversized := openTestWAL(t, other, 1, options)
	if _, err := oversized.Append(context.Background(), make([]byte, maximum+1)); !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("oversized append error=%v", err)
	}
	if matches, err := filepath.Glob(filepath.Join(other, "segment-*.wal")); err != nil || len(matches) != 0 {
		t.Fatalf("oversized append created segment: %v err=%v", matches, err)
	}
}

func makeThreeSegmentWAL(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	options := testOptions()
	options.SyncBytes = 1
	wal := openTestWAL(t, dir, 1, options)
	for _, payload := range []string{"one", "two", "three"} {
		if _, err := wal.Append(context.Background(), []byte(payload)); err != nil {
			t.Fatal(err)
		}
	}
	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}
