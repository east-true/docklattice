package serverapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/east-true/dockpilot/internal/producttransport"
	"github.com/east-true/dockpilot/internal/webui"
)

const (
	queryContainerList = "container.list"
	queryImageList     = "image.list"
	queryNetworkList   = "network.list"
	queryVolumeList    = "volume.list"
	maxInventoryItems  = 10_000
	maxImageReferences = 256
)

var hostInventoryTimeout = 5 * time.Second

type agentContainer struct {
	ID     string            `json:"id"`
	Names  []string          `json:"names"`
	Image  string            `json:"image"`
	State  string            `json:"state"`
	Status string            `json:"status"`
	Labels map[string]string `json:"labels,omitempty"`
	Mounts []agentMount      `json:"mounts"`
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
		if !validDockerObjectID(item.ID) || !validInventoryStrings(item.Names, 256, 4096) ||
			!strictlySortedStrings(item.Names) || !validInventoryString(item.Image, 4096, true) ||
			!validInventoryString(item.State, 128, true) || !validInventoryString(item.Status, 4096, false) ||
			!validContainerDetails(item) {
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
		}
		clearContainerDetails(item)
	}
	return containers, nil
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

func validContainerDetails(item *agentContainer) bool {
	if len(item.Labels) > 1024 || len(item.Mounts) > 1024 {
		return false
	}
	for key, value := range item.Labels {
		if !validInventoryString(key, 4096, true) || !validInventoryString(value, 4096, false) {
			return false
		}
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

func clearContainerDetails(item *agentContainer) {
	for key := range item.Labels {
		delete(item.Labels, key)
	}
	for index := range item.Mounts {
		item.Mounts[index].Source = ""
		item.Mounts[index].Destination = ""
	}
	clear(item.Mounts)
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
