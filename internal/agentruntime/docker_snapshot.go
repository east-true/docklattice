package agentruntime

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"sort"
	"strconv"

	"github.com/east-true/dockpilot/internal/auditevents"
	"github.com/east-true/dockpilot/internal/auditgen"
	"github.com/east-true/dockpilot/internal/dockeradapter"
)

const dockerSnapshotOncePrefix = "docker-snapshot:"

// reconcileDockerSnapshot persists only a digest of the current container
// inventory. A difference after a prior snapshot is an uncertainty signal,
// never a reconstructed Docker state or a guessed exact event sequence.
func (r *Runtime) reconcileDockerSnapshot(ctx context.Context) error {
	if r.docker == nil {
		return nil
	}
	containers, err := r.docker.List(ctx)
	if err != nil {
		return fmt.Errorf("agentruntime: reconcile Docker snapshot: %w", err)
	}
	digest := dockerSnapshotDigest(containers)
	snapshot, err := r.state.Snapshot()
	if err != nil {
		return err
	}
	key := dockerSnapshotOncePrefix + digest
	if snapshot.DockerSnapshotSHA256 == digest {
		return r.wal.ForgetOnce(key)
	}
	if snapshot.DockerSnapshotSHA256 == "" {
		return r.state.SetDockerSnapshotSHA256(ctx, digest)
	}
	at := r.config.Now().UTC()
	payload, err := auditevents.Encode(auditgen.Event{
		Kind: auditgen.KindObserved, ResourceType: "docker", ResourceID: "inventory",
		Action: "unobserved_change", FirstAt: at, LastAt: at, Count: 1,
		Attributes: map[string]string{
			"previous_snapshot_sha256": snapshot.DockerSnapshotSHA256,
			"current_snapshot_sha256":  digest,
			"container_count":          strconv.Itoa(len(containers)),
		},
	})
	if err != nil {
		return err
	}
	if _, err := r.wal.AppendOnce(ctx, key, payload); err != nil {
		return fmt.Errorf("agentruntime: append Docker snapshot reconciliation Audit: %w", err)
	}
	if err := r.state.SetDockerSnapshotSHA256(ctx, digest); err != nil {
		return err
	}
	return r.wal.ForgetOnce(key)
}

func dockerSnapshotDigest(containers []dockeradapter.Container) string {
	ordered := append([]dockeradapter.Container(nil), containers...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].ID != ordered[j].ID {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].Image < ordered[j].Image
	})
	digest := sha256.New()
	writeSnapshotField(digest, strconv.Itoa(len(ordered)))
	for _, container := range ordered {
		writeSnapshotField(digest, container.ID)
		writeSnapshotField(digest, container.Image)
		writeSnapshotField(digest, container.State)
		writeSnapshotField(digest, container.Status)
		writeSnapshotField(digest, container.Health)
		writeSnapshotField(digest, strconv.Itoa(container.ExitCode))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func writeSnapshotField(destination hash.Hash, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = destination.Write(length[:])
	_, _ = destination.Write([]byte(value))
}
