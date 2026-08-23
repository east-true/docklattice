package dockeradapter

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/east-true/dockpilot/internal/agentsafety"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

const (
	protectedID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	workloadID  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type fakeEngine struct {
	ping          client.PingResult
	pingOptions   client.PingOptions
	pingErr       error
	version       client.ServerVersionResult
	versionErr    error
	list          client.ContainerListResult
	listOptions   client.ContainerListOptions
	listErr       error
	inspect       client.ContainerInspectResult
	inspectID     string
	inspectErr    error
	mutation      string
	mutationID    string
	removeOptions client.ContainerRemoveOptions
	mutationErr   error
	closed        bool
}

func (f *fakeEngine) Ping(_ context.Context, options client.PingOptions) (client.PingResult, error) {
	f.pingOptions = options
	return f.ping, f.pingErr
}
func (f *fakeEngine) ServerVersion(context.Context, client.ServerVersionOptions) (client.ServerVersionResult, error) {
	return f.version, f.versionErr
}
func (f *fakeEngine) ContainerList(_ context.Context, options client.ContainerListOptions) (client.ContainerListResult, error) {
	f.listOptions = options
	return f.list, f.listErr
}
func (f *fakeEngine) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	f.inspectID = id
	return f.inspect, f.inspectErr
}
func (f *fakeEngine) ContainerStart(_ context.Context, id string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	f.mutation, f.mutationID = "start", id
	return client.ContainerStartResult{}, f.mutationErr
}
func (f *fakeEngine) ContainerStop(_ context.Context, id string, _ client.ContainerStopOptions) (client.ContainerStopResult, error) {
	f.mutation, f.mutationID = "stop", id
	return client.ContainerStopResult{}, f.mutationErr
}
func (f *fakeEngine) ContainerRestart(_ context.Context, id string, _ client.ContainerRestartOptions) (client.ContainerRestartResult, error) {
	f.mutation, f.mutationID = "restart", id
	return client.ContainerRestartResult{}, f.mutationErr
}
func (f *fakeEngine) ContainerRemove(_ context.Context, id string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.mutation, f.mutationID, f.removeOptions = "remove", id, options
	return client.ContainerRemoveResult{}, f.mutationErr
}
func (f *fakeEngine) Close() error { f.closed = true; return nil }

type inventoryFakeEngine struct {
	*fakeEngine
	images   client.ImageListResult
	networks client.NetworkListResult
	volumes  client.VolumeListResult
}

func (f *inventoryFakeEngine) ImageList(_ context.Context, _ client.ImageListOptions) (client.ImageListResult, error) {
	return f.images, nil
}
func (f *inventoryFakeEngine) NetworkList(_ context.Context, _ client.NetworkListOptions) (client.NetworkListResult, error) {
	return f.networks, nil
}
func (f *inventoryFakeEngine) VolumeList(_ context.Context, _ client.VolumeListOptions) (client.VolumeListResult, error) {
	return f.volumes, nil
}

func identified() agentsafety.Identification {
	return agentsafety.Identification{ProtectedContainerIDs: []string{protectedID}}
}

func openFake(t *testing.T, api engine, identity agentsafety.Identification) *Adapter {
	t.Helper()
	adapter, err := New(api, func() agentsafety.Identification { return identity }, MinimumAPIVersion)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func TestProbeNegotiatesAndChecksDeclaredMinimum(t *testing.T) {
	engine := &fakeEngine{ping: client.PingResult{APIVersion: "1.55"}, version: client.ServerVersionResult{Version: "29.0.0", APIVersion: "1.55"}}
	adapter := openFake(t, engine, identified())
	capability, err := adapter.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !engine.pingOptions.NegotiateAPIVersion || !capability.Available || capability.MinimumAPI != MinimumAPIVersion || capability.EngineVersion != "29.0.0" {
		t.Fatalf("probe = %+v options=%+v", capability, engine.pingOptions)
	}

	engine.version.APIVersion = "1.39"
	capability, err = adapter.Probe(context.Background())
	if !errors.Is(err, ErrUnsupportedVersion) || capability.Available {
		t.Fatalf("old Engine probe = %+v, %v", capability, err)
	}
}

func TestProbeSeparatesUnavailability(t *testing.T) {
	sentinel := errors.New("socket missing")
	engine := &fakeEngine{pingErr: sentinel}
	adapter := openFake(t, engine, identified())
	capability, err := adapter.Probe(context.Background())
	if !errors.Is(err, ErrUnavailable) || capability.Available || capability.Reason == "" {
		t.Fatalf("probe = %+v, %v", capability, err)
	}
}

func TestListMapsAndDefensivelyCopiesRawFacts(t *testing.T) {
	labels := map[string]string{"com.docker.compose.project": "demo"}
	engine := &fakeEngine{list: client.ContainerListResult{Items: []container.Summary{{
		ID: workloadID, Names: []string{"/demo-web-1"}, Image: "demo:1", State: container.StateRunning,
		Status: "Up", Labels: labels, Mounts: []container.MountPoint{
			{Type: "bind", Source: "/srv", Destination: "/work", RW: true},
			{Type: "volume", Name: "demo-data", Source: "/var/lib/docker/volumes/demo-data/_data", Destination: "/data", RW: true},
		},
	}}}}
	adapter := openFake(t, engine, identified())
	got, err := adapter.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !engine.listOptions.All || len(got) != 1 || got[0].ID != workloadID || got[0].Labels["com.docker.compose.project"] != "demo" ||
		!got[0].Mounts[0].ReadWrite || got[0].Mounts[1].Source != "demo-data" {
		t.Fatalf("List = %+v options=%+v", got, engine.listOptions)
	}
	got[0].Labels["com.docker.compose.project"] = "mutated"
	got[0].Names[0] = "mutated"
	if labels["com.docker.compose.project"] != "demo" || engine.list.Items[0].Names[0] != "/demo-web-1" {
		t.Fatal("caller mutated Engine result")
	}
}

func TestReadOnlyInventoryMapsOnlyBoundedOverviewFacts(t *testing.T) {
	api := &inventoryFakeEngine{fakeEngine: &fakeEngine{},
		images: client.ImageListResult{Items: []image.Summary{{
			ID: "sha256:" + workloadID, RepoTags: []string{"demo:latest"}, RepoDigests: []string{"demo@sha256:" + workloadID},
			Created: 10, Size: 20, Containers: 1, Labels: map[string]string{"secret": "not-forwarded"},
		}}},
		networks: client.NetworkListResult{Items: []network.Summary{{Network: network.Network{
			ID: workloadID, Name: "demo", Driver: "bridge", Scope: "local", Internal: true,
			Labels: map[string]string{"secret": "not-forwarded"}, Options: map[string]string{"secret": "not-forwarded"},
		}}}},
		volumes: client.VolumeListResult{Items: []volume.Volume{{
			Name: "data", Driver: "local", Scope: "local", CreatedAt: "2026-08-15T00:00:00Z",
			Mountpoint: "/private/host/path", Labels: map[string]string{"secret": "not-forwarded"},
		}}},
	}
	adapter := openFake(t, api, identified())
	images, err := adapter.ListImages(context.Background())
	if err != nil || len(images) != 1 || images[0].ID != "sha256:"+workloadID || images[0].RepoTags[0] != "demo:latest" {
		t.Fatalf("images = %+v, %v", images, err)
	}
	networks, err := adapter.ListNetworks(context.Background())
	if err != nil || len(networks) != 1 || networks[0].Name != "demo" || !networks[0].Internal {
		t.Fatalf("networks = %+v, %v", networks, err)
	}
	volumes, err := adapter.ListVolumes(context.Background())
	if err != nil || len(volumes) != 1 || volumes[0].Name != "data" {
		t.Fatalf("volumes = %+v, %v", volumes, err)
	}
	if strings.Contains(fmt.Sprintf("%+v", volumes), "/private/host/path") {
		t.Fatal("volume inventory exposed host mountpoint")
	}
}

func TestMutationsEnforceSelfProtectionAndForwardClosedOptions(t *testing.T) {
	engine := &fakeEngine{}
	adapter := openFake(t, engine, identified())
	if err := adapter.Start(context.Background(), protectedID); err != nil || engine.mutation != "start" {
		t.Fatalf("protected start = %q, %v", engine.mutation, err)
	}
	engine.mutation = ""
	for name, call := range map[string]func() error{
		"stop":    func() error { return adapter.Stop(context.Background(), protectedID) },
		"restart": func() error { return adapter.Restart(context.Background(), protectedID) },
		"remove":  func() error { return adapter.Remove(context.Background(), protectedID, RemoveOptions{}) },
	} {
		if err := call(); !errors.Is(err, ErrMutationDenied) {
			t.Errorf("%s protected error = %v", name, err)
		}
		if engine.mutation != "" {
			t.Errorf("%s reached Engine", name)
		}
	}
	if err := adapter.Remove(context.Background(), workloadID, RemoveOptions{RemoveVolumes: true, Force: true}); err != nil {
		t.Fatal(err)
	}
	if engine.mutation != "remove" || engine.mutationID != workloadID || !reflect.DeepEqual(engine.removeOptions, client.ContainerRemoveOptions{RemoveVolumes: true, Force: true}) {
		t.Fatalf("remove = %q %q %+v", engine.mutation, engine.mutationID, engine.removeOptions)
	}
}

func TestFailClosedAndInvalidIDNeverReachEngine(t *testing.T) {
	engine := &fakeEngine{}
	adapter := openFake(t, engine, agentsafety.Identification{FailClosed: true})
	if err := adapter.Start(context.Background(), workloadID); !errors.Is(err, ErrMutationDenied) {
		t.Fatalf("fail-closed start = %v", err)
	}
	if err := adapter.Stop(context.Background(), "short-id"); !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("invalid target = %v", err)
	}
	if _, err := adapter.Inspect(context.Background(), "--help"); !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("invalid inspect = %v", err)
	}
	if engine.mutation != "" || engine.inspectID != "" {
		t.Fatal("rejected request reached Engine")
	}
}

func TestAPIVersionComparisonIsNumeric(t *testing.T) {
	for _, test := range []struct {
		actual, minimum string
		want            bool
	}{
		{"1.40", "1.40", true}, {"1.55", "1.9", true}, {"2.0", "1.99", true}, {"1.9", "1.40", false},
	} {
		got, err := apiAtLeast(test.actual, test.minimum)
		if err != nil || got != test.want {
			t.Errorf("apiAtLeast(%q,%q) = %v,%v", test.actual, test.minimum, got, err)
		}
	}
	if _, _, err := parseAPIVersion("latest"); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("parse error = %v", err)
	}
}
