package agentsafety

import (
	"sort"
	"strings"
)

const (
	AgentRoleLabel      = "io.dockpilot.role"
	AgentRoleValue      = "agent"
	ComposeProjectLabel = "com.docker.compose.project"
)

// Container is the identity input available from Docker inspection/listing.
type Container struct {
	ID     string
	Name   string
	Labels map[string]string
}

// SelfConfig is the explicit fallback used only when no role-labelled Agent
// container can be found.
type SelfConfig struct {
	ContainerID   string
	ContainerName string
}

// IdentificationSource records which precedence branch selected the Agent.
type IdentificationSource string

const (
	IdentificationByLabel  IdentificationSource = "LABEL"
	IdentificationByConfig IdentificationSource = "CONFIG"
	IdentificationFailed   IdentificationSource = "FAILED"
)

// Identification contains the complete protection set derived from all
// selected Agent containers and their Compose projects.
type Identification struct {
	Source                   IdentificationSource
	FailClosed               bool
	SelectedAgentIDs         []string
	ProtectedContainerIDs    []string
	ProtectedComposeProjects []string
	Reason                   string
}

// IdentifySelf applies label-first identification. All labelled Agents are
// selected. Configuration is a fallback only, and multiple matching fallback
// containers are conservatively protected together.
func IdentifySelf(containers []Container, cfg SelfConfig) Identification {
	selected := make(map[string]struct{})
	for _, container := range containers {
		if container.ID != "" && container.Labels[AgentRoleLabel] == AgentRoleValue {
			selected[container.ID] = struct{}{}
		}
	}
	source := IdentificationByLabel
	if len(selected) == 0 {
		source = IdentificationByConfig
		configuredName := normalizeContainerName(cfg.ContainerName)
		for _, container := range containers {
			if container.ID == "" {
				continue
			}
			idMatch := cfg.ContainerID != "" && container.ID == cfg.ContainerID
			nameMatch := configuredName != "" && normalizeContainerName(container.Name) == configuredName
			if idMatch || nameMatch {
				selected[container.ID] = struct{}{}
			}
		}
	}
	if len(selected) == 0 {
		return Identification{
			Source:     IdentificationFailed,
			FailClosed: true,
			Reason:     "no Agent container matched the role label or configured ID/name",
		}
	}

	projects := make(map[string]struct{})
	protected := make(map[string]struct{}, len(selected))
	for _, container := range containers {
		if _, ok := selected[container.ID]; !ok {
			continue
		}
		protected[container.ID] = struct{}{}
		if project := strings.TrimSpace(container.Labels[ComposeProjectLabel]); project != "" {
			projects[project] = struct{}{}
		}
	}
	for _, container := range containers {
		if _, ok := projects[strings.TrimSpace(container.Labels[ComposeProjectLabel])]; ok && container.ID != "" {
			protected[container.ID] = struct{}{}
		}
	}

	return Identification{
		Source:                   source,
		SelectedAgentIDs:         sortedKeys(selected),
		ProtectedContainerIDs:    sortedKeys(protected),
		ProtectedComposeProjects: sortedKeys(projects),
		Reason:                   "Agent identity and Compose protection set established",
	}
}

func normalizeContainerName(name string) string {
	return strings.TrimLeft(strings.TrimSpace(name), "/")
}

func sortedKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
