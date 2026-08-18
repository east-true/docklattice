package agentsafety

import (
	"reflect"
	"testing"
)

func TestIdentifySelfLabelPrecedenceAndProjectProtection(t *testing.T) {
	containers := []Container{
		{ID: "configured", Name: "configured-agent"},
		{ID: "agent-b", Name: "/agent-b", Labels: map[string]string{AgentRoleLabel: AgentRoleValue, ComposeProjectLabel: "dockpilot-b"}},
		{ID: "sibling-b", Labels: map[string]string{ComposeProjectLabel: "dockpilot-b"}},
		{ID: "agent-a", Labels: map[string]string{AgentRoleLabel: AgentRoleValue, ComposeProjectLabel: "dockpilot-a"}},
		{ID: "unrelated", Labels: map[string]string{ComposeProjectLabel: "other"}},
	}
	got := IdentifySelf(containers, SelfConfig{ContainerID: "configured"})
	if got.Source != IdentificationByLabel || got.FailClosed {
		t.Fatalf("identity = %#v", got)
	}
	if !reflect.DeepEqual(got.SelectedAgentIDs, []string{"agent-a", "agent-b"}) {
		t.Fatalf("selected = %v", got.SelectedAgentIDs)
	}
	if !reflect.DeepEqual(got.ProtectedContainerIDs, []string{"agent-a", "agent-b", "sibling-b"}) {
		t.Fatalf("protected containers = %v", got.ProtectedContainerIDs)
	}
	if !reflect.DeepEqual(got.ProtectedComposeProjects, []string{"dockpilot-a", "dockpilot-b"}) {
		t.Fatalf("protected projects = %v", got.ProtectedComposeProjects)
	}
}

func TestIdentifySelfFallsBackToConfiguredIDAndName(t *testing.T) {
	containers := []Container{
		{ID: "by-id", Name: "first", Labels: map[string]string{ComposeProjectLabel: "id-project"}},
		{ID: "by-name", Name: "/configured", Labels: map[string]string{ComposeProjectLabel: "name-project"}},
		{ID: "sibling", Labels: map[string]string{ComposeProjectLabel: "name-project"}},
	}
	got := IdentifySelf(containers, SelfConfig{ContainerID: "by-id", ContainerName: "configured"})
	if got.Source != IdentificationByConfig || got.FailClosed {
		t.Fatalf("identity = %#v", got)
	}
	if !reflect.DeepEqual(got.SelectedAgentIDs, []string{"by-id", "by-name"}) {
		t.Fatalf("selected = %v", got.SelectedAgentIDs)
	}
	if !reflect.DeepEqual(got.ProtectedContainerIDs, []string{"by-id", "by-name", "sibling"}) {
		t.Fatalf("protected = %v", got.ProtectedContainerIDs)
	}
}

func TestIdentifySelfFailureIsFailClosed(t *testing.T) {
	got := IdentifySelf([]Container{{ID: "other", Name: "other"}}, SelfConfig{ContainerName: "missing"})
	if got.Source != IdentificationFailed || !got.FailClosed || got.Reason == "" {
		t.Fatalf("identity = %#v", got)
	}
}
