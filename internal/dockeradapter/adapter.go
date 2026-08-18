// Package dockeradapter provides the product's narrow Docker Engine boundary.
// Compose operations deliberately do not pass through this package.
package dockeradapter

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/east-true/dockpilot/internal/agentsafety"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

const MinimumAPIVersion = "1.40"

var (
	ErrUnavailable        = errors.New("DOCKER_UNAVAILABLE")
	ErrUnsupportedVersion = errors.New("DOCKER_API_UNSUPPORTED")
	ErrInvalidTarget      = errors.New("DOCKER_INVALID_TARGET")
	ErrMutationDenied     = errors.New("DOCKER_MUTATION_DENIED")
	containerIDPattern    = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type engine interface {
	Ping(context.Context, client.PingOptions) (client.PingResult, error)
	ServerVersion(context.Context, client.ServerVersionOptions) (client.ServerVersionResult, error)
	ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerRestart(context.Context, string, client.ContainerRestartOptions) (client.ContainerRestartResult, error)
	ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	Close() error
}

// inventoryEngine is optional at the constructor boundary so focused Docker
// mutation fakes do not have to pretend to implement unrelated read APIs. The
// production Moby client implements the complete interface.
type inventoryEngine interface {
	ImageList(context.Context, client.ImageListOptions) (client.ImageListResult, error)
	NetworkList(context.Context, client.NetworkListOptions) (client.NetworkListResult, error)
	VolumeList(context.Context, client.VolumeListOptions) (client.VolumeListResult, error)
}

var _ inventoryEngine = (*client.Client)(nil)

type IdentityProvider func() agentsafety.Identification

type Adapter struct {
	engine     engine
	identity   IdentityProvider
	minimumAPI string
}

type Capability struct {
	Available        bool
	EngineVersion    string
	ServerAPIVersion string
	MinimumAPI       string
	Reason           string
}

type Container struct {
	ID       string
	Names    []string
	Image    string
	State    string
	Status   string
	Health   string
	ExitCode int
	Labels   map[string]string
	Mounts   []Mount
}

type Mount struct {
	Type        string
	Source      string
	Destination string
	ReadWrite   bool
}

type Image struct {
	ID          string
	RepoTags    []string
	RepoDigests []string
	Created     int64
	Size        int64
	Containers  int64
}

type Network struct {
	ID         string
	Name       string
	Driver     string
	Scope      string
	Internal   bool
	Attachable bool
	Ingress    bool
}

type Volume struct {
	Name      string
	Driver    string
	Scope     string
	CreatedAt string
}

type RemoveOptions struct {
	RemoveVolumes bool
	Force         bool
}

// OpenFromEnv uses the official Moby client. API negotiation is enabled by
// default in current clients and is explicitly requested by Probe as well.
func OpenFromEnv(identity IdentityProvider) (*Adapter, error) {
	engine, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("%w: create Engine client: %v", ErrUnavailable, err)
	}
	return New(engine, identity, MinimumAPIVersion)
}

func New(api engine, identity IdentityProvider, minimumAPI string) (*Adapter, error) {
	if api == nil || identity == nil {
		return nil, fmt.Errorf("%w: Engine and identity provider are required", ErrUnavailable)
	}
	if minimumAPI == "" {
		minimumAPI = MinimumAPIVersion
	}
	if _, _, err := parseAPIVersion(minimumAPI); err != nil {
		return nil, err
	}
	return &Adapter{engine: api, identity: identity, minimumAPI: minimumAPI}, nil
}

func (adapter *Adapter) Close() error { return adapter.engine.Close() }

// Probe is a startup gate, not a background retry loop. It records the
// negotiated server capability separately from Compose capability.
func (adapter *Adapter) Probe(ctx context.Context) (Capability, error) {
	ping, err := adapter.engine.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true})
	if err != nil {
		return Capability{MinimumAPI: adapter.minimumAPI, Reason: err.Error()}, fmt.Errorf("%w: ping: %v", ErrUnavailable, err)
	}
	version, err := adapter.engine.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return Capability{MinimumAPI: adapter.minimumAPI, ServerAPIVersion: ping.APIVersion, Reason: err.Error()}, fmt.Errorf("%w: server version: %v", ErrUnavailable, err)
	}
	serverAPI := version.APIVersion
	if serverAPI == "" {
		serverAPI = ping.APIVersion
	}
	compatible, err := apiAtLeast(serverAPI, adapter.minimumAPI)
	if err != nil || !compatible {
		reason := fmt.Sprintf("server API %q is below required %s", serverAPI, adapter.minimumAPI)
		return Capability{EngineVersion: version.Version, ServerAPIVersion: serverAPI, MinimumAPI: adapter.minimumAPI, Reason: reason}, fmt.Errorf("%w: %s", ErrUnsupportedVersion, reason)
	}
	return Capability{Available: true, EngineVersion: version.Version, ServerAPIVersion: serverAPI, MinimumAPI: adapter.minimumAPI}, nil
}

func (adapter *Adapter) List(ctx context.Context) ([]Container, error) {
	result, err := adapter.engine.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("%w: list containers: %v", ErrUnavailable, err)
	}
	containers := make([]Container, 0, len(result.Items))
	for _, item := range result.Items {
		containers = append(containers, fromSummary(item))
	}
	return containers, nil
}

func (adapter *Adapter) ListImages(ctx context.Context) ([]Image, error) {
	api, ok := adapter.engine.(inventoryEngine)
	if !ok {
		return nil, fmt.Errorf("%w: image inventory API is unavailable", ErrUnavailable)
	}
	result, err := api.ImageList(ctx, client.ImageListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("%w: list images: %v", ErrUnavailable, err)
	}
	images := make([]Image, len(result.Items))
	for index, item := range result.Items {
		images[index] = fromImageSummary(item)
	}
	return images, nil
}

func (adapter *Adapter) ListNetworks(ctx context.Context) ([]Network, error) {
	api, ok := adapter.engine.(inventoryEngine)
	if !ok {
		return nil, fmt.Errorf("%w: network inventory API is unavailable", ErrUnavailable)
	}
	result, err := api.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w: list networks: %v", ErrUnavailable, err)
	}
	networks := make([]Network, len(result.Items))
	for index, item := range result.Items {
		networks[index] = fromNetworkSummary(item)
	}
	return networks, nil
}

func (adapter *Adapter) ListVolumes(ctx context.Context) ([]Volume, error) {
	api, ok := adapter.engine.(inventoryEngine)
	if !ok {
		return nil, fmt.Errorf("%w: volume inventory API is unavailable", ErrUnavailable)
	}
	result, err := api.VolumeList(ctx, client.VolumeListOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w: list volumes: %v", ErrUnavailable, err)
	}
	volumes := make([]Volume, len(result.Items))
	for index, item := range result.Items {
		volumes[index] = fromVolume(item)
	}
	return volumes, nil
}

func (adapter *Adapter) Inspect(ctx context.Context, id string) (Container, error) {
	if err := validateContainerID(id); err != nil {
		return Container{}, err
	}
	result, err := adapter.engine.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return Container{}, fmt.Errorf("docker inspect %s: %w", id, err)
	}
	return fromInspect(result.Container), nil
}

func (adapter *Adapter) Start(ctx context.Context, id string) error {
	if err := adapter.authorize(id, agentsafety.ActionContainerStart); err != nil {
		return err
	}
	if _, err := adapter.engine.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("docker start %s: %w", id, err)
	}
	return nil
}

func (adapter *Adapter) Stop(ctx context.Context, id string) error {
	if err := adapter.authorize(id, agentsafety.ActionContainerStop); err != nil {
		return err
	}
	if _, err := adapter.engine.ContainerStop(ctx, id, client.ContainerStopOptions{}); err != nil {
		return fmt.Errorf("docker stop %s: %w", id, err)
	}
	return nil
}

func (adapter *Adapter) Restart(ctx context.Context, id string) error {
	if err := adapter.authorize(id, agentsafety.ActionContainerRestart); err != nil {
		return err
	}
	if _, err := adapter.engine.ContainerRestart(ctx, id, client.ContainerRestartOptions{}); err != nil {
		return fmt.Errorf("docker restart %s: %w", id, err)
	}
	return nil
}

func (adapter *Adapter) Remove(ctx context.Context, id string, options RemoveOptions) error {
	if err := adapter.authorize(id, agentsafety.ActionContainerRemove); err != nil {
		return err
	}
	if _, err := adapter.engine.ContainerRemove(ctx, id, client.ContainerRemoveOptions{RemoveVolumes: options.RemoveVolumes, Force: options.Force}); err != nil {
		return fmt.Errorf("docker remove %s: %w", id, err)
	}
	return nil
}

func (adapter *Adapter) authorize(id string, kind agentsafety.ActionKind) error {
	if err := validateContainerID(id); err != nil {
		return err
	}
	decision := agentsafety.Decide(adapter.identity(), agentsafety.Action{Kind: kind, ContainerID: id})
	if !decision.Allowed {
		return fmt.Errorf("%w: %s: %s", ErrMutationDenied, decision.Code, decision.Reason)
	}
	return nil
}

func validateContainerID(id string) error {
	if !containerIDPattern.MatchString(id) {
		return fmt.Errorf("%w: require canonical 64-character container ID", ErrInvalidTarget)
	}
	return nil
}

func fromSummary(value container.Summary) Container {
	mounts := make([]Mount, 0, len(value.Mounts))
	for _, mount := range value.Mounts {
		mounts = append(mounts, Mount{Type: string(mount.Type), Source: mount.Source, Destination: mount.Destination, ReadWrite: mount.RW})
	}
	result := Container{ID: value.ID, Names: append([]string(nil), value.Names...), Image: value.Image, State: string(value.State), Status: value.Status, Labels: cloneLabels(value.Labels), Mounts: mounts}
	if value.Health != nil {
		result.Health = string(value.Health.Status)
	}
	return result
}

func fromInspect(value container.InspectResponse) Container {
	result := Container{ID: value.ID, Image: value.Image}
	if value.Name != "" {
		result.Names = []string{value.Name}
	}
	if value.Config != nil {
		result.Labels = cloneLabels(value.Config.Labels)
		if result.Image == "" {
			result.Image = value.Config.Image
		}
	}
	if value.State != nil {
		result.State = string(value.State.Status)
		result.Status = string(value.State.Status)
		result.ExitCode = value.State.ExitCode
		if value.State.Health != nil {
			result.Health = string(value.State.Health.Status)
		}
	}
	for _, mount := range value.Mounts {
		result.Mounts = append(result.Mounts, Mount{Type: string(mount.Type), Source: mount.Source, Destination: mount.Destination, ReadWrite: mount.RW})
	}
	return result
}

func cloneLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return nil
	}
	copy := make(map[string]string, len(labels))
	for key, value := range labels {
		copy[key] = value
	}
	return copy
}

func fromImageSummary(value image.Summary) Image {
	return Image{
		ID: value.ID, RepoTags: append([]string(nil), value.RepoTags...), RepoDigests: append([]string(nil), value.RepoDigests...),
		Created: value.Created, Size: value.Size, Containers: value.Containers,
	}
}

func fromNetworkSummary(value network.Summary) Network {
	return Network{
		ID: value.ID, Name: value.Name, Driver: value.Driver, Scope: value.Scope,
		Internal: value.Internal, Attachable: value.Attachable, Ingress: value.Ingress,
	}
}

func fromVolume(value volume.Volume) Volume {
	return Volume{Name: value.Name, Driver: value.Driver, Scope: value.Scope, CreatedAt: value.CreatedAt}
}

func apiAtLeast(actual, minimum string) (bool, error) {
	actualMajor, actualMinor, err := parseAPIVersion(actual)
	if err != nil {
		return false, err
	}
	minimumMajor, minimumMinor, err := parseAPIVersion(minimum)
	if err != nil {
		return false, err
	}
	return actualMajor > minimumMajor || (actualMajor == minimumMajor && actualMinor >= minimumMinor), nil
}

func parseAPIVersion(value string) (int, int, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("%w: invalid API version %q", ErrUnsupportedVersion, value)
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	if errMajor != nil || errMinor != nil || major < 0 || minor < 0 {
		return 0, 0, fmt.Errorf("%w: invalid API version %q", ErrUnsupportedVersion, value)
	}
	return major, minor, nil
}
