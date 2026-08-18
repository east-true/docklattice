package serverapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/producttransport"
	"github.com/east-true/dockpilot/internal/webui"
)

func TestHostInventoryIsTypedLiveOnlyAndDoesNotPersistSecrets(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	session := newFakeSession("agent-a")
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}
	secret := "inventory-secret-do-not-store"

	session.setQueryPayload([]byte(`[{"id":"` + strings.Repeat("a", 64) + `","names":["/app"],"image":"repo/app:latest","state":"running","status":"Up","labels":{"token":"` + secret + `"},"mounts":[{"type":"bind","source":"/srv/` + secret + `","destination":"/app/config","read_write":false}]}]`))
	containers, err := backend.HostContainers(ctx, "agent-a")
	if err != nil || len(containers) != 1 || containers[0].Image != "repo/app:latest" {
		t.Fatalf("containers = %+v, %v", containers, err)
	}
	assertInventoryQuery(t, session, queryContainerList)
	encoded, err := json.Marshal(containers)
	if err != nil || strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "labels") || strings.Contains(string(encoded), "mount") {
		t.Fatalf("curated container response leaked raw details: %s, %v", encoded, err)
	}

	session.setQueryPayload([]byte(`[{"id":"sha256:` + strings.Repeat("b", 64) + `","repo_tags":["repo/app:latest"],"repo_digests":["repo/app@sha256:` + strings.Repeat("c", 64) + `"],"created_unix":1,"size_bytes":2,"containers":1}]`))
	images, err := backend.HostImages(ctx, "agent-a")
	if err != nil || len(images) != 1 || images[0].Size != 2 {
		t.Fatalf("images = %+v, %v", images, err)
	}
	assertInventoryQuery(t, session, queryImageList)

	session.setQueryPayload([]byte(`[{"id":"` + strings.Repeat("d", 64) + `","name":"bridge","driver":"bridge","scope":"local","internal":false,"attachable":false,"ingress":false}]`))
	networks, err := backend.HostNetworks(ctx, "agent-a")
	if err != nil || len(networks) != 1 || networks[0].Driver != "bridge" {
		t.Fatalf("networks = %+v, %v", networks, err)
	}
	assertInventoryQuery(t, session, queryNetworkList)

	session.setQueryPayload([]byte(`[{"name":"data","driver":"local","scope":"local","created_at":"2026-08-15T00:00:00Z"}]`))
	volumes, err := backend.HostVolumes(ctx, "agent-a")
	if err != nil || len(volumes) != 1 || volumes[0].Name != "data" {
		t.Fatalf("volumes = %+v, %v", volumes, err)
	}
	assertInventoryQuery(t, session, queryVolumeList)

	assertNoPersistentSecret(t, ctx, store, secret)
}

func TestHostInventoryRejectsInvalidAgentResponses(t *testing.T) {
	ctx, backend, _, registry := newTestBackend(t)
	session := newFakeSession("agent-a")
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 64)
	tests := []struct {
		name    string
		payload []byte
		call    func() error
	}{
		{"image raw labels", []byte(`[{"id":"` + id + `","repo_tags":[],"repo_digests":[],"created_unix":0,"size_bytes":0,"containers":-1,"labels":{"secret":"x"}}]`), func() error { _, err := backend.HostImages(ctx, "agent-a"); return err }},
		{"network raw options", []byte(`[{"id":"` + id + `","name":"n","driver":"bridge","scope":"local","internal":false,"attachable":false,"ingress":false,"options":{"secret":"x"}}]`), func() error { _, err := backend.HostNetworks(ctx, "agent-a"); return err }},
		{"volume mountpoint", []byte(`[{"name":"v","driver":"local","scope":"local","mountpoint":"/secret"}]`), func() error { _, err := backend.HostVolumes(ctx, "agent-a"); return err }},
		{"volume options", []byte(`[{"name":"v","driver":"local","scope":"local","options":{"device":"secret"}}]`), func() error { _, err := backend.HostVolumes(ctx, "agent-a"); return err }},
		{"unsorted images", []byte(`[{"id":"` + strings.Repeat("b", 64) + `","repo_tags":[],"repo_digests":[],"created_unix":0,"size_bytes":0,"containers":-1},{"id":"` + id + `","repo_tags":[],"repo_digests":[],"created_unix":0,"size_bytes":0,"containers":-1}]`), func() error { _, err := backend.HostImages(ctx, "agent-a"); return err }},
		{"duplicate image reference", []byte(`[{"id":"` + id + `","repo_tags":["repo/app:latest","repo/app:latest"],"repo_digests":[],"created_unix":0,"size_bytes":0,"containers":-1}]`), func() error { _, err := backend.HostImages(ctx, "agent-a"); return err }},
		{"duplicate container name", []byte(`[{"id":"` + id + `","names":["/app","/app"],"image":"repo/app:latest","state":"running","status":"Up","mounts":[]}]`), func() error { _, err := backend.HostContainers(ctx, "agent-a"); return err }},
		{"invalid utf8", append([]byte(`[{"name":"`), append([]byte{0xff}, []byte(`","driver":"local","scope":"local"}]`)...)...), func() error { _, err := backend.HostVolumes(ctx, "agent-a"); return err }},
		{"oversized", make([]byte, producttransport.DefaultMaxMessageBytes+1), func() error { _, err := backend.HostImages(ctx, "agent-a"); return err }},
	}
	tooMany, err := json.Marshal(make([]webui.HostVolume, maxInventoryItems+1))
	if err != nil {
		t.Fatal(err)
	}
	tests = append(tests, struct {
		name    string
		payload []byte
		call    func() error
	}{"too many", tooMany, func() error { _, err := backend.HostVolumes(ctx, "agent-a"); return err }})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session.setQueryPayload(test.payload)
			if err := test.call(); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("error = %v, want corrupt Agent response", err)
			}
		})
	}
}

func TestHostInventoryOfflineUnavailableAndCancelled(t *testing.T) {
	ctx, backend, _, registry := newTestBackend(t)
	if _, err := backend.HostImages(ctx, "agent-offline"); !errors.Is(err, ErrAgentOffline) || !errors.Is(err, webui.ErrUnavailable) {
		t.Fatalf("offline error = %v", err)
	}
	if _, err := backend.HostImages(ctx, "bad/id"); !errors.Is(err, webui.ErrInvalidRequest) {
		t.Fatalf("invalid Agent ID error = %v", err)
	}

	session := newFakeSession("agent-a")
	session.queryErr = producttransport.ErrHandlerUnavailable
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.HostImages(ctx, "agent-a"); !errors.Is(err, producttransport.ErrHandlerUnavailable) || !errors.Is(err, webui.ErrUnavailable) {
		t.Fatalf("N-1 handler error = %v", err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	queries := session.queries
	if _, err := backend.HostVolumes(cancelled, "agent-a"); !errors.Is(err, context.Canceled) || session.queries != queries {
		t.Fatalf("cancelled query error=%v calls=%d want=%d", err, session.queries, queries)
	}
}

type deadlineInventorySession struct{ *fakeSession }

func (s *deadlineInventorySession) Query(ctx context.Context, _ producttransport.QueryRequest) (producttransport.QueryResponse, error) {
	<-ctx.Done()
	return producttransport.QueryResponse{}, ctx.Err()
}

func TestHostInventoryHasIndependentFiveSecondBound(t *testing.T) {
	ctx, backend, _, registry := newTestBackend(t)
	session := &deadlineInventorySession{fakeSession: newFakeSession("agent-a")}
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}
	previous := hostInventoryTimeout
	hostInventoryTimeout = 10 * time.Millisecond
	t.Cleanup(func() { hostInventoryTimeout = previous })
	started := time.Now()
	_, err := backend.HostImages(ctx, "agent-a")
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, webui.ErrUnavailable) {
		t.Fatalf("deadline error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded inventory query took %v", elapsed)
	}
}

func assertInventoryQuery(t *testing.T, session *fakeSession, kind string) {
	t.Helper()
	request := session.lastQuery()
	if request.Kind != kind || request.Target != "" || len(request.Payload) != 0 {
		t.Fatalf("query = %+v, want typed empty %q request", request, kind)
	}
}
