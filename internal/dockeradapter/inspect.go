package dockeradapter

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/moby/moby/client"
)

var volumeObjectPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$`)

type detailEngine interface {
	ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error)
	NetworkInspect(context.Context, string, client.NetworkInspectOptions) (client.NetworkInspectResult, error)
	VolumeInspect(context.Context, string, client.VolumeInspectOptions) (client.VolumeInspectResult, error)
}

var _ detailEngine = (*client.Client)(nil)

type ImageDetails struct {
	ID, Created, Author, Architecture, Variant, OS, OSVersion string
	Size                                                      int64
	RepoTags, RepoDigests, Entrypoint, Command, ExposedPorts  []string
	WorkingDir, User                                          string
	Labels                                                    map[string]string
	LayerCount                                                int
}

type IPAMConfig struct {
	Subnet, IPRange, Gateway string
	AuxAddresses             map[string]string
}

type NetworkEndpoint struct {
	ContainerID, Name, EndpointID, IPv4, IPv6, MAC string
}

type NetworkDetails struct {
	ID, Name, Created, Scope, Driver                                  string
	EnableIPv4, EnableIPv6, Internal, Attachable, Ingress, ConfigOnly bool
	IPAMDriver                                                        string
	IPAM                                                              []IPAMConfig
	Options, Labels                                                   map[string]string
	Containers                                                        []NetworkEndpoint
}

type VolumeDetails struct {
	Name, Driver, Scope, CreatedAt, Mountpoint string
	Options, Labels                            map[string]string
}

func (adapter *Adapter) InspectImage(ctx context.Context, id string) (ImageDetails, error) {
	if !containerIDPattern.MatchString(strings.TrimPrefix(id, "sha256:")) {
		return ImageDetails{}, fmt.Errorf("%w: canonical Image ID is required", ErrInvalidTarget)
	}
	api, ok := adapter.engine.(detailEngine)
	if !ok {
		return ImageDetails{}, fmt.Errorf("%w: image inspect API is unavailable", ErrUnavailable)
	}
	result, err := api.ImageInspect(ctx, id)
	if err != nil {
		return ImageDetails{}, fmt.Errorf("%w: inspect image: %v", ErrUnavailable, err)
	}
	details := ImageDetails{ID: result.ID, Created: result.Created, Author: result.Author, Architecture: result.Architecture,
		Variant: result.Variant, OS: result.Os, OSVersion: result.OsVersion, Size: result.Size,
		RepoTags: append([]string(nil), result.RepoTags...), RepoDigests: append([]string(nil), result.RepoDigests...), LayerCount: len(result.RootFS.Layers)}
	if result.Config != nil {
		details.Entrypoint = append([]string(nil), result.Config.Entrypoint...)
		details.Command = append([]string(nil), result.Config.Cmd...)
		details.WorkingDir, details.User = result.Config.WorkingDir, result.Config.User
		details.Labels = cloneLabels(result.Config.Labels)
		for port := range result.Config.ExposedPorts {
			details.ExposedPorts = append(details.ExposedPorts, port)
		}
	}
	return details, nil
}

func (adapter *Adapter) InspectNetwork(ctx context.Context, id string) (NetworkDetails, error) {
	if !containerIDPattern.MatchString(id) {
		return NetworkDetails{}, fmt.Errorf("%w: canonical Network ID is required", ErrInvalidTarget)
	}
	api, ok := adapter.engine.(detailEngine)
	if !ok {
		return NetworkDetails{}, fmt.Errorf("%w: network inspect API is unavailable", ErrUnavailable)
	}
	result, err := api.NetworkInspect(ctx, id, client.NetworkInspectOptions{})
	if err != nil {
		return NetworkDetails{}, fmt.Errorf("%w: inspect network: %v", ErrUnavailable, err)
	}
	value := result.Network
	details := NetworkDetails{ID: value.ID, Name: value.Name, Created: value.Created.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		Scope: value.Scope, Driver: value.Driver, EnableIPv4: value.EnableIPv4, EnableIPv6: value.EnableIPv6,
		Internal: value.Internal, Attachable: value.Attachable, Ingress: value.Ingress, ConfigOnly: value.ConfigOnly,
		IPAMDriver: value.IPAM.Driver, Options: cloneLabels(value.Options), Labels: cloneLabels(value.Labels)}
	for _, config := range value.IPAM.Config {
		entry := IPAMConfig{AuxAddresses: make(map[string]string, len(config.AuxAddress))}
		if config.Subnet.IsValid() {
			entry.Subnet = config.Subnet.String()
		}
		if config.IPRange.IsValid() {
			entry.IPRange = config.IPRange.String()
		}
		if config.Gateway.IsValid() {
			entry.Gateway = config.Gateway.String()
		}
		for name, address := range config.AuxAddress {
			if address.IsValid() {
				entry.AuxAddresses[name] = address.String()
			}
		}
		details.IPAM = append(details.IPAM, entry)
	}
	for containerID, endpoint := range value.Containers {
		entry := NetworkEndpoint{ContainerID: containerID, Name: endpoint.Name, EndpointID: endpoint.EndpointID, MAC: endpoint.MacAddress.String()}
		if endpoint.IPv4Address.IsValid() {
			entry.IPv4 = endpoint.IPv4Address.String()
		}
		if endpoint.IPv6Address.IsValid() {
			entry.IPv6 = endpoint.IPv6Address.String()
		}
		details.Containers = append(details.Containers, entry)
	}
	return details, nil
}

func (adapter *Adapter) InspectVolume(ctx context.Context, name string) (VolumeDetails, error) {
	if !volumeObjectPattern.MatchString(name) {
		return VolumeDetails{}, fmt.Errorf("%w: valid Volume name is required", ErrInvalidTarget)
	}
	api, ok := adapter.engine.(detailEngine)
	if !ok {
		return VolumeDetails{}, fmt.Errorf("%w: volume inspect API is unavailable", ErrUnavailable)
	}
	result, err := api.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err != nil {
		return VolumeDetails{}, fmt.Errorf("%w: inspect volume: %v", ErrUnavailable, err)
	}
	value := result.Volume
	return VolumeDetails{Name: value.Name, Driver: value.Driver, Scope: value.Scope, CreatedAt: value.CreatedAt,
		Mountpoint: value.Mountpoint, Options: cloneLabels(value.Options), Labels: cloneLabels(value.Labels)}, nil
}
