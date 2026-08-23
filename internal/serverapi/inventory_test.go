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

func TestHostObjectInspectorsAreBoundedLiveAndCurated(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	session := newFakeSession("agent-a")
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}
	containerID := strings.Repeat("a", 64)
	imageID := "sha256:" + strings.Repeat("b", 64)
	networkID := strings.Repeat("c", 64)
	secret := "inspector-secret-do-not-store"

	session.setQueryPayload([]byte(`{"id":"` + containerID + `","names":["/api"],"image":"demo/api:1","image_id":"` + imageID + `","state":"running","status":"running","labels":{"token":"` + secret + `"},"mounts":[{"type":"volume","source":"data","destination":"/data","read_write":true}],"ports":[],"exit_code":0,"created_at":"2026-08-23T00:00:00Z","started_at":"2026-08-23T00:00:01Z","restart_count":1,"restart_policy":"unless-stopped","logging_driver":"json-file","command":["serve"],"entrypoint":["/app"],"exposed_ports":["8080/tcp"],"networks":[{"name":"default","network_id":"` + networkID + `","endpoint_id":"` + strings.Repeat("d", 64) + `","ipv4":"172.30.0.2","mac":"02:42:ac:1e:00:02","aliases":["api"]}]}`))
	container, err := backend.HostContainer(ctx, "agent-a", containerID)
	if err != nil || container.Labels["token"] != secret || len(container.Networks) != 1 || container.RestartPolicy != "unless-stopped" {
		t.Fatalf("Container Inspector = %+v, %v", container, err)
	}
	assertObjectQuery(t, session, "container.inspect", containerID)

	session.setQueryPayload([]byte(`{"id":"` + imageID + `","repo_tags":["demo/api:1"],"repo_digests":[],"created":"2026-08-23T00:00:00Z","architecture":"amd64","os":"linux","size_bytes":42,"exposed_ports":["8080/tcp"],"labels":{"token":"` + secret + `"},"layer_count":3,"used_by":[{"container_id":"` + containerID + `","container_name":"api","compose_project":"demo","compose_service":"api"}]}`))
	image, err := backend.HostImage(ctx, "agent-a", imageID)
	if err != nil || image.LayerCount != 3 || len(image.UsedBy) != 1 || image.Labels["token"] != secret {
		t.Fatalf("Image Inspector = %+v, %v", image, err)
	}
	assertObjectQuery(t, session, "image.inspect", imageID)

	session.setQueryPayload([]byte(`{"id":"` + networkID + `","name":"demo_default","created":"2026-08-23T00:00:00Z","scope":"local","driver":"bridge","enable_ipv4":true,"enable_ipv6":false,"internal":false,"attachable":false,"ingress":false,"config_only":false,"ipam_driver":"default","ipam":[{"subnet":"172.30.0.0/16","gateway":"172.30.0.1"}],"options":{"secret":"` + secret + `"},"labels":{},"compose_project":"demo","compose_network":"default","attachments":[{"container_id":"` + containerID + `","container_name":"api"}]}`))
	network, err := backend.HostNetwork(ctx, "agent-a", networkID)
	if err != nil || network.ComposeProject != "demo" || len(network.IPAM) != 1 || network.Options["secret"] != secret {
		t.Fatalf("Network Inspector = %+v, %v", network, err)
	}
	assertObjectQuery(t, session, "network.inspect", networkID)

	session.setQueryPayload([]byte(`{"name":"data","driver":"local","scope":"local","created_at":"2026-08-23T00:00:00Z","mountpoint":"/var/lib/docker/volumes/data/_data","options":{},"labels":{"token":"` + secret + `"},"compose_project":"demo","compose_volume":"data","references":[{"container_id":"` + containerID + `","container_name":"api","destination":"/data"}]}`))
	volume, err := backend.HostVolume(ctx, "agent-a", "data")
	if err != nil || volume.Mountpoint == "" || volume.ComposeVolume != "data" || len(volume.References) != 1 {
		t.Fatalf("Volume Inspector = %+v, %v", volume, err)
	}
	assertObjectQuery(t, session, "volume.inspect", "data")

	assertNoPersistentSecret(t, ctx, store, secret)
}

func TestHostObjectInspectorsRejectInvalidOrMismatchedResponses(t *testing.T) {
	ctx, backend, _, registry := newTestBackend(t)
	session := newFakeSession("agent-a")
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 64)
	for _, test := range []struct {
		name, payload string
		call          func() error
	}{
		{"Container mismatch", `{"id":"` + strings.Repeat("b", 64) + `","names":[],"image":"x","state":"running","status":"running","mounts":[],"ports":[]}`, func() error { _, err := backend.HostContainer(ctx, "agent-a", id); return err }},
		{"Image unsorted ports", `{"id":"` + id + `","repo_tags":[],"repo_digests":[],"size_bytes":0,"exposed_ports":["9/tcp","8/tcp"],"layer_count":0,"used_by":[]}`, func() error { _, err := backend.HostImage(ctx, "agent-a", id); return err }},
		{"Network invalid IPAM", `{"id":"` + id + `","name":"n","scope":"local","driver":"bridge","ipam":[{"subnet":"not-a-prefix"}],"attachments":[]}`, func() error { _, err := backend.HostNetwork(ctx, "agent-a", id); return err }},
		{"Volume invalid reference", `{"name":"data","driver":"local","scope":"local","references":[{"container_id":"short"}]}`, func() error { _, err := backend.HostVolume(ctx, "agent-a", "data"); return err }},
	} {
		t.Run(test.name, func(t *testing.T) {
			session.setQueryPayload([]byte(test.payload))
			if err := test.call(); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("error = %v, want corrupt Agent response", err)
			}
		})
	}
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

func assertObjectQuery(t *testing.T, session *fakeSession, kind, target string) {
	t.Helper()
	request := session.lastQuery()
	if request.Kind != kind || request.Target != target || len(request.Payload) != 0 {
		t.Fatalf("query = %+v, want typed %q request for %q", request, kind, target)
	}
}
