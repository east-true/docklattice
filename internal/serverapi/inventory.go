package serverapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/east-true/docklattice/internal/producttransport"
	"github.com/east-true/docklattice/internal/webui"
)

const (
	queryContainerList = "container.list"
	queryImageList     = "image.list"
	queryNetworkList   = "network.list"
	queryVolumeList    = "volume.list"
	maxInventoryItems  = 10_000
	maxImageReferences = 256
)

var (
	hostInventoryTimeout = 5 * time.Second
	volumeObjectName     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$`)
)

type agentContainer struct {
	ID                  string                  `json:"id"`
	Names               []string                `json:"names"`
	Image               string                  `json:"image"`
	State               string                  `json:"state"`
	Status              string                  `json:"status"`
	Labels              map[string]string       `json:"labels,omitempty"`
	Mounts              []agentMount            `json:"mounts"`
	Health              string                  `json:"health,omitempty"`
	ComposeProject      string                  `json:"compose_project,omitempty"`
	ComposeService      string                  `json:"compose_service,omitempty"`
	OneOff              bool                    `json:"one_off"`
	Ports               []webui.PublishedPort   `json:"ports"`
	Protected           bool                    `json:"protected"`
	ProtectionReason    string                  `json:"protection_reason,omitempty"`
	ImageID             string                  `json:"image_id,omitempty"`
	ExitCode            int                     `json:"exit_code"`
	CreatedAt           string                  `json:"created_at,omitempty"`
	StartedAt           string                  `json:"started_at,omitempty"`
	FinishedAt          string                  `json:"finished_at,omitempty"`
	OOMKilled           bool                    `json:"oom_killed"`
	RestartCount        int                     `json:"restart_count"`
	RestartPolicy       string                  `json:"restart_policy,omitempty"`
	RestartMaximumRetry int                     `json:"restart_maximum_retry"`
	StopSignal          string                  `json:"stop_signal,omitempty"`
	StopTimeout         *int                    `json:"stop_timeout_seconds,omitempty"`
	LoggingDriver       string                  `json:"logging_driver,omitempty"`
	Command             []string                `json:"command,omitempty"`
	Entrypoint          []string                `json:"entrypoint,omitempty"`
	ExposedPorts        []string                `json:"exposed_ports,omitempty"`
	Networks            []agentContainerNetwork `json:"networks,omitempty"`
}

type agentContainerNetwork struct {
	Name       string   `json:"name"`
	NetworkID  string   `json:"network_id,omitempty"`
	EndpointID string   `json:"endpoint_id,omitempty"`
	IPv4       string   `json:"ipv4,omitempty"`
	IPv6       string   `json:"ipv6,omitempty"`
	MAC        string   `json:"mac,omitempty"`
	Aliases    []string `json:"aliases,omitempty"`
}

type agentMount struct {
	Type        string `json:"type"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	ReadWrite   bool   `json:"read_write"`
}

func (b *Backend) HostContainers(ctx context.Context, agentID string) ([]webui.HostContainer, error) {
	var decoded []agentContainer
	if err := b.queryHostInventory(ctx, agentID, queryContainerList, "container inventory", &decoded); err != nil {
		clearAgentContainers(decoded)
		return nil, err
	}
	defer clearAgentContainers(decoded)
	containers := make([]webui.HostContainer, len(decoded))
	seen := make(map[string]struct{}, len(decoded))
	previous := ""
	for index := range decoded {
		item := &decoded[index]
		if !validContainerBase(item) || !validContainerDetails(item) {
			clearContainerDetails(item)
			return nil, invalidInventory("container", index)
		}
		if _, duplicate := seen[item.ID]; duplicate || index > 0 && item.ID <= previous {
			clearContainerDetails(item)
			return nil, invalidInventory("container", index)
		}
		seen[item.ID] = struct{}{}
		previous = item.ID
		containers[index] = webui.HostContainer{
			ID: item.ID, Names: append([]string(nil), item.Names...), Image: item.Image, State: item.State, Status: item.Status,
			Health: item.Health, ComposeProject: item.ComposeProject, ComposeService: item.ComposeService, OneOff: item.OneOff,
			Ports: append([]webui.PublishedPort(nil), item.Ports...), Protected: item.Protected, ProtectionReason: item.ProtectionReason,
		}
		clearContainerDetails(item)
	}
	return containers, nil
}

func (b *Backend) HostContainer(ctx context.Context, agentID, containerID string) (webui.HostContainer, error) {
	if !canonicalContainerID.MatchString(containerID) {
		return webui.HostContainer{}, fmt.Errorf("%w: canonical Container ID is required", webui.ErrInvalidRequest)
	}
	var item agentContainer
	if err := b.queryHostObject(ctx, agentID, "container.inspect", containerID, "Container details", &item); err != nil {
		return webui.HostContainer{}, err
	}
	defer clearContainerDetails(&item)
	if item.ID != containerID || !validContainerBase(&item) || !validContainerDetails(&item) || !validContainerInspect(&item) {
		return webui.HostContainer{}, invalidInventory("Container details", 0)
	}
	result := webContainer(item)
	result.Labels = cloneMap(item.Labels)
	result.Mounts = make([]webui.ContainerMount, len(item.Mounts))
	for index, mount := range item.Mounts {
		result.Mounts[index] = webui.ContainerMount{Type: mount.Type, Source: mount.Source, Destination: mount.Destination, ReadWrite: mount.ReadWrite}
	}
	result.Networks = make([]webui.ContainerNetwork, len(item.Networks))
	for index, network := range item.Networks {
		result.Networks[index] = webui.ContainerNetwork{Name: network.Name, NetworkID: network.NetworkID, EndpointID: network.EndpointID, IPv4: network.IPv4, IPv6: network.IPv6, MAC: network.MAC, Aliases: append([]string(nil), network.Aliases...)}
	}
	return result, nil
}

func webContainer(item agentContainer) webui.HostContainer {
	var stopTimeout *int
	if item.StopTimeout != nil {
		value := *item.StopTimeout
		stopTimeout = &value
	}
	return webui.HostContainer{ID: item.ID, Names: append([]string(nil), item.Names...), Image: item.Image, ImageID: item.ImageID, State: item.State, Status: item.Status, Health: item.Health,
		ComposeProject: item.ComposeProject, ComposeService: item.ComposeService, OneOff: item.OneOff, Ports: append([]webui.PublishedPort(nil), item.Ports...), Protected: item.Protected, ProtectionReason: item.ProtectionReason,
		ExitCode: item.ExitCode, CreatedAt: item.CreatedAt, StartedAt: item.StartedAt, FinishedAt: item.FinishedAt, OOMKilled: item.OOMKilled, RestartCount: item.RestartCount, RestartPolicy: item.RestartPolicy,
		RestartMaximumRetry: item.RestartMaximumRetry, StopSignal: item.StopSignal, StopTimeout: stopTimeout, LoggingDriver: item.LoggingDriver, Command: append([]string(nil), item.Command...), Entrypoint: append([]string(nil), item.Entrypoint...), ExposedPorts: append([]string(nil), item.ExposedPorts...)}
}

func (b *Backend) HostImages(ctx context.Context, agentID string) ([]webui.HostImage, error) {
	var images []webui.HostImage
	if err := b.queryHostInventory(ctx, agentID, queryImageList, "image inventory", &images); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(images))
	previous := ""
	for index := range images {
		image := &images[index]
		identity := strings.TrimPrefix(image.ID, "sha256:")
		if !validDockerObjectID(image.ID) || image.Created < 0 || image.Size < 0 || image.Containers < -1 ||
			!validInventoryStrings(image.RepoTags, maxImageReferences, 4096) ||
			!validInventoryStrings(image.RepoDigests, maxImageReferences, 4096) ||
			!strictlySortedStrings(image.RepoTags) || !strictlySortedStrings(image.RepoDigests) {
			return nil, invalidInventory("image", index)
		}
		if _, duplicate := seen[identity]; duplicate || index > 0 && image.ID <= previous {
			return nil, invalidInventory("image", index)
		}
		seen[identity] = struct{}{}
		previous = image.ID
	}
	return images, nil
}

func (b *Backend) HostImage(ctx context.Context, agentID, imageID string) (webui.HostImageDetails, error) {
	if !validDockerObjectID(imageID) {
		return webui.HostImageDetails{}, fmt.Errorf("%w: canonical Image ID is required", webui.ErrInvalidRequest)
	}
	var result webui.HostImageDetails
	if err := b.queryHostObject(ctx, agentID, "image.inspect", imageID, "Image details", &result); err != nil {
		return webui.HostImageDetails{}, err
	}
	if !validImageInspect(result) || strings.TrimPrefix(result.ID, "sha256:") != strings.TrimPrefix(imageID, "sha256:") {
		return webui.HostImageDetails{}, invalidInventory("Image details", 0)
	}
	return result, nil
}

func (b *Backend) HostNetworks(ctx context.Context, agentID string) ([]webui.HostNetwork, error) {
	var networks []webui.HostNetwork
	if err := b.queryHostInventory(ctx, agentID, queryNetworkList, "network inventory", &networks); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(networks))
	previous := ""
	for index := range networks {
		network := &networks[index]
		identity := strings.TrimPrefix(network.ID, "sha256:")
		if !validDockerObjectID(network.ID) || !validInventoryString(network.Name, 255, true) ||
			!validInventoryString(network.Driver, 128, true) || !validInventoryString(network.Scope, 64, true) {
			return nil, invalidInventory("network", index)
		}
		if _, duplicate := seen[identity]; duplicate || index > 0 && network.ID <= previous {
			return nil, invalidInventory("network", index)
		}
		seen[identity] = struct{}{}
		previous = network.ID
	}
	return networks, nil
}

func (b *Backend) HostNetwork(ctx context.Context, agentID, networkID string) (webui.HostNetworkDetails, error) {
	if !canonicalContainerID.MatchString(networkID) {
		return webui.HostNetworkDetails{}, fmt.Errorf("%w: canonical Network ID is required", webui.ErrInvalidRequest)
	}
	var result webui.HostNetworkDetails
	if err := b.queryHostObject(ctx, agentID, "network.inspect", networkID, "Network details", &result); err != nil {
		return webui.HostNetworkDetails{}, err
	}
	if result.ID != networkID || !validNetworkInspect(result) {
		return webui.HostNetworkDetails{}, invalidInventory("Network details", 0)
	}
	return result, nil
}

func (b *Backend) HostVolumes(ctx context.Context, agentID string) ([]webui.HostVolume, error) {
	var volumes []webui.HostVolume
	if err := b.queryHostInventory(ctx, agentID, queryVolumeList, "volume inventory", &volumes); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(volumes))
	previous := ""
	for index := range volumes {
		volume := &volumes[index]
		if !validInventoryString(volume.Name, 255, true) || !validInventoryString(volume.Driver, 128, true) ||
			!validInventoryString(volume.Scope, 64, true) || !validInventoryString(volume.CreatedAt, 128, false) {
			return nil, invalidInventory("volume", index)
		}
		if _, duplicate := seen[volume.Name]; duplicate || index > 0 && volume.Name <= previous {
			return nil, invalidInventory("volume", index)
		}
		seen[volume.Name] = struct{}{}
		previous = volume.Name
	}
	return volumes, nil
}

func (b *Backend) HostVolume(ctx context.Context, agentID, name string) (webui.HostVolumeDetails, error) {
	if !volumeObjectName.MatchString(name) {
		return webui.HostVolumeDetails{}, fmt.Errorf("%w: valid Volume name is required", webui.ErrInvalidRequest)
	}
	var result webui.HostVolumeDetails
	if err := b.queryHostObject(ctx, agentID, "volume.inspect", name, "Volume details", &result); err != nil {
		return webui.HostVolumeDetails{}, err
	}
	if result.Name != name || !validVolumeInspect(result) {
		return webui.HostVolumeDetails{}, invalidInventory("Volume details", 0)
	}
	return result, nil
}

func (b *Backend) queryHostObject(ctx context.Context, agentID, kind, target, action string, value any) error {
	if !validOpaqueID(agentID) {
		return fmt.Errorf("%w: valid Agent ID is required", webui.ErrInvalidRequest)
	}
	session, err := b.activeSession(agentID)
	if err != nil {
		return err
	}
	queryCtx, cancel := context.WithTimeout(ctx, hostInventoryTimeout)
	defer cancel()
	response, err := session.Query(queryCtx, producttransport.QueryRequest{Kind: kind, Target: target})
	defer clear(response.Payload)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &liveUnavailableError{agentID: agentID, action: action, cause: err}
	}
	if err := queryCtx.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &liveUnavailableError{agentID: agentID, action: action, cause: err}
	}
	if len(response.Payload) > producttransport.DefaultMaxMessageBytes {
		return &corruptDataError{boundary: "Agent " + action + " response", cause: errors.New("payload exceeds transport limit")}
	}
	if err := decodeStrictJSON(response.Payload, value); err != nil {
		return &corruptDataError{boundary: "Agent " + action + " response", cause: err}
	}
	return nil
}

func (b *Backend) queryHostInventory(ctx context.Context, agentID, kind, action string, target any) error {
	if !validOpaqueID(agentID) {
		return fmt.Errorf("%w: valid Agent ID is required", webui.ErrInvalidRequest)
	}
	session, err := b.activeSession(agentID)
	if err != nil {
		return err
	}
	queryCtx, cancel := context.WithTimeout(ctx, hostInventoryTimeout)
	defer cancel()
	if err := queryCtx.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &liveUnavailableError{agentID: agentID, action: action, cause: err}
	}
	response, err := session.Query(queryCtx, producttransport.QueryRequest{Kind: kind})
	defer clear(response.Payload)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &liveUnavailableError{agentID: agentID, action: action, cause: err}
	}
	if err := queryCtx.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &liveUnavailableError{agentID: agentID, action: action, cause: err}
	}
	if len(response.Payload) > producttransport.DefaultMaxMessageBytes {
		return &corruptDataError{boundary: "Agent " + action + " response", cause: errors.New("payload exceeds transport limit")}
	}
	if err := decodeStrictJSON(response.Payload, target); err != nil {
		return &corruptDataError{boundary: "Agent " + action + " response", cause: err}
	}
	count, ok := inventoryCount(target)
	if !ok || count > maxInventoryItems {
		return &corruptDataError{boundary: "Agent " + action + " response", cause: errors.New("item count exceeds limit")}
	}
	return nil
}

func inventoryCount(value any) (int, bool) {
	switch items := value.(type) {
	case *[]webui.HostImage:
		return len(*items), true
	case *[]agentContainer:
		return len(*items), true
	case *[]webui.HostNetwork:
		return len(*items), true
	case *[]webui.HostVolume:
		return len(*items), true
	default:
		return 0, false
	}
}

func validContainerBase(item *agentContainer) bool {
	return validDockerObjectID(item.ID) && validInventoryStrings(item.Names, 256, 4096) && strictlySortedStrings(item.Names) &&
		validInventoryString(item.Image, 4096, true) && validInventoryString(item.State, 128, true) && validInventoryString(item.Status, 4096, false)
}

func validContainerDetails(item *agentContainer) bool {
	if len(item.Labels) > 1024 || len(item.Mounts) > 1024 || len(item.Ports) > 1024 ||
		!validInventoryString(item.Health, 128, false) || !validInventoryString(item.ComposeProject, 255, false) ||
		!validInventoryString(item.ComposeService, 255, false) || !validInventoryString(item.ProtectionReason, 1024, false) {
		return false
	}
	for key, value := range item.Labels {
		if !validInventoryString(key, 4096, true) || !validInventoryString(value, 4096, false) {
			return false
		}
	}
	previousPort := ""
	for _, port := range item.Ports {
		if port.TargetPort == 0 || (port.Protocol != "tcp" && port.Protocol != "udp" && port.Protocol != "sctp") ||
			!validInventoryString(port.HostIP, 64, false) {
			return false
		}
		key := fmt.Sprintf("%05d/%05d/%s/%s", port.TargetPort, port.PublishedPort, port.Protocol, port.HostIP)
		if previousPort != "" && key < previousPort {
			return false
		}
		previousPort = key
	}
	previousDestination, previousSource := "", ""
	for index := range item.Mounts {
		mount := &item.Mounts[index]
		if !validInventoryString(mount.Type, 128, true) || !validInventoryString(mount.Source, 4096, false) ||
			!validInventoryString(mount.Destination, 4096, true) {
			return false
		}
		if index > 0 && (mount.Destination < previousDestination ||
			mount.Destination == previousDestination && mount.Source < previousSource) {
			return false
		}
		previousDestination, previousSource = mount.Destination, mount.Source
	}
	return true
}

func validContainerInspect(item *agentContainer) bool {
	if item.ExitCode < 0 || item.RestartCount < 0 || item.RestartMaximumRetry < 0 || item.StopTimeout != nil && *item.StopTimeout < 0 ||
		!validInventoryString(item.ImageID, 128, false) || !validInventoryString(item.RestartPolicy, 64, false) ||
		!validInventoryString(item.StopSignal, 64, false) || !validInventoryString(item.LoggingDriver, 128, false) ||
		!validInventoryStrings(item.Command, 1024, 4096) || !validInventoryStrings(item.Entrypoint, 1024, 4096) ||
		!validInventoryStrings(item.ExposedPorts, 1024, 128) || len(item.Networks) > 1024 {
		return false
	}
	for _, value := range []string{item.CreatedAt, item.StartedAt, item.FinishedAt} {
		if value != "" {
			if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
				return false
			}
		}
	}
	previous := ""
	for _, network := range item.Networks {
		if !validInventoryString(network.Name, 255, true) || !validOptionalCanonicalID(network.NetworkID) || !validOptionalCanonicalID(network.EndpointID) || !validOptionalAddress(network.IPv4) || !validOptionalAddress(network.IPv6) || !validOptionalMAC(network.MAC) || !validInventoryStrings(network.Aliases, 256, 255) || !strictlySortedStrings(network.Aliases) || previous != "" && network.Name <= previous {
			return false
		}
		previous = network.Name
	}
	return true
}

func validImageInspect(item webui.HostImageDetails) bool {
	if !validDockerObjectID(item.ID) || item.Size < 0 || item.LayerCount < 0 || item.LayerCount > 100_000 ||
		!validInventoryStrings(item.RepoTags, maxImageReferences, 4096) || !strictlySortedStrings(item.RepoTags) ||
		!validInventoryStrings(item.RepoDigests, maxImageReferences, 4096) || !strictlySortedStrings(item.RepoDigests) ||
		!validOptionalTime(item.Created) || !validInventoryString(item.Author, 4096, false) ||
		!validInventoryString(item.Architecture, 128, false) || !validInventoryString(item.Variant, 128, false) ||
		!validInventoryString(item.OS, 128, false) || !validInventoryString(item.OSVersion, 1024, false) ||
		!validInventoryStrings(item.Entrypoint, 1024, 4096) || !validInventoryStrings(item.Command, 1024, 4096) ||
		!validInventoryStrings(item.ExposedPorts, 1024, 128) || !strictlySortedStrings(item.ExposedPorts) ||
		!validInventoryString(item.WorkingDir, 4096, false) || !validInventoryString(item.User, 1024, false) ||
		!validStringMap(item.Labels) || !validObjectReferences(item.UsedBy) {
		return false
	}
	return true
}

func validNetworkInspect(item webui.HostNetworkDetails) bool {
	if !canonicalContainerID.MatchString(item.ID) || !validInventoryString(item.Name, 255, true) || !validOptionalTime(item.Created) ||
		!validInventoryString(item.Driver, 128, true) || !validInventoryString(item.Scope, 64, true) ||
		!validInventoryString(item.IPAMDriver, 128, false) || !validInventoryString(item.ComposeProject, 255, false) ||
		!validInventoryString(item.ComposeNetwork, 255, false) || len(item.IPAM) > 256 || !validStringMap(item.Options) ||
		!validStringMap(item.Labels) || !validNetworkAttachments(item.Attachments) {
		return false
	}
	for _, config := range item.IPAM {
		if !validOptionalPrefix(config.Subnet) || !validOptionalPrefix(config.IPRange) || !validOptionalAddress(config.Gateway) || !validAddressMap(config.AuxAddresses) {
			return false
		}
	}
	return true
}

func validVolumeInspect(item webui.HostVolumeDetails) bool {
	return volumeObjectName.MatchString(item.Name) && validInventoryString(item.Driver, 128, true) &&
		validInventoryString(item.Scope, 64, true) && validOptionalTime(item.CreatedAt) && validInventoryString(item.Mountpoint, 4096, false) &&
		validInventoryString(item.ComposeProject, 255, false) && validInventoryString(item.ComposeVolume, 255, false) &&
		validStringMap(item.Options) && validStringMap(item.Labels) && validObjectReferences(item.References)
}

func validOptionalTime(value string) bool {
	if value == "" {
		return true
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && !parsed.IsZero()
}

func validOptionalCanonicalID(value string) bool {
	return value == "" || canonicalContainerID.MatchString(value)
}
func validOptionalPrefix(value string) bool {
	if value == "" {
		return true
	}
	_, err := netip.ParsePrefix(value)
	return err == nil
}
func validOptionalAddress(value string) bool {
	if value == "" {
		return true
	}
	_, err := netip.ParseAddr(value)
	return err == nil
}
func validOptionalMAC(value string) bool {
	if value == "" {
		return true
	}
	_, err := net.ParseMAC(value)
	return err == nil
}
func validAddressMap(value map[string]string) bool {
	if len(value) > 1024 {
		return false
	}
	for key, address := range value {
		if !validInventoryString(key, 255, true) || !validOptionalAddress(address) || address == "" {
			return false
		}
	}
	return true
}

func validStringMap(value map[string]string) bool {
	if len(value) > 1024 {
		return false
	}
	for key, item := range value {
		if !validInventoryString(key, 4096, true) || !validInventoryString(item, 4096, false) {
			return false
		}
	}
	return true
}
func cloneMap(value map[string]string) map[string]string {
	if value == nil {
		return nil
	}
	result := make(map[string]string, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}
func validObjectReferences(values []webui.ObjectReference) bool {
	if len(values) > maxInventoryItems {
		return false
	}
	previous := ""
	for _, value := range values {
		if !canonicalContainerID.MatchString(value.ContainerID) || !validInventoryString(value.ContainerName, 4096, false) || !validInventoryString(value.ComposeProject, 255, false) || !validInventoryString(value.ComposeService, 255, false) || !validInventoryString(value.State, 128, false) || !validInventoryString(value.Destination, 4096, false) {
			return false
		}
		key := value.ContainerID + "\x00" + value.Destination
		if previous != "" && key <= previous {
			return false
		}
		previous = key
	}
	return true
}

func validNetworkAttachments(values []webui.NetworkAttachment) bool {
	if len(values) > maxInventoryItems {
		return false
	}
	previous := ""
	for _, value := range values {
		if !canonicalContainerID.MatchString(value.ContainerID) || !validInventoryString(value.ContainerName, 4096, false) ||
			!validInventoryString(value.ComposeProject, 255, false) || !validInventoryString(value.ComposeService, 255, false) || !validInventoryString(value.State, 128, false) ||
			!validOptionalCanonicalID(value.EndpointID) || !validOptionalPrefix(value.IPv4) || !validOptionalPrefix(value.IPv6) ||
			!validOptionalMAC(value.MAC) || previous != "" && value.ContainerID <= previous {
			return false
		}
		previous = value.ContainerID
	}
	return true
}

func clearContainerDetails(item *agentContainer) {
	for key := range item.Labels {
		delete(item.Labels, key)
	}
	for index := range item.Mounts {
		item.Mounts[index].Source = ""
		item.Mounts[index].Destination = ""
	}
	clear(item.Mounts)
	clear(item.Command)
	clear(item.Entrypoint)
	clear(item.ExposedPorts)
	for index := range item.Networks {
		clear(item.Networks[index].Aliases)
		item.Networks[index].Aliases = nil
	}
	clear(item.Networks)
}

func clearAgentContainers(items []agentContainer) {
	for index := range items {
		clearContainerDetails(&items[index])
	}
	clear(items)
}

func validDockerObjectID(value string) bool {
	return canonicalSHA256.MatchString(strings.TrimPrefix(value, "sha256:"))
}

func validInventoryString(value string, maximum int, required bool) bool {
	return (!required || value != "") && len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func validInventoryStrings(values []string, maxCount, maxBytes int) bool {
	if len(values) > maxCount {
		return false
	}
	for _, value := range values {
		if !validInventoryString(value, maxBytes, false) {
			return false
		}
	}
	return true
}

func strictlySortedStrings(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index] <= values[index-1] {
			return false
		}
	}
	return true
}

func invalidInventory(kind string, index int) error {
	return &corruptDataError{boundary: "Agent " + kind + " inventory response", cause: fmt.Errorf("invalid item %d", index)}
}
