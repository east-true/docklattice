package agentsafety

import "testing"

func TestDecide(t *testing.T) {
	identified := Identification{
		ProtectedContainerIDs:    []string{"agent", "agent-sidecar"},
		ProtectedComposeProjects: []string{"dockpilot"},
	}
	failClosed := Identification{FailClosed: true}
	tests := []struct {
		name     string
		identity Identification
		action   Action
		allowed  bool
		code     DecisionCode
	}{
		{"query protected", identified, Action{Kind: ActionQuery, ContainerID: "agent"}, true, DecisionAllowReadOnly},
		{"start protected", identified, Action{Kind: ActionContainerStart, ContainerID: "agent"}, true, DecisionAllowUnprotected},
		{"start fail closed", failClosed, Action{Kind: ActionContainerStart, ContainerID: "workload"}, false, DecisionDenyFailClosed},
		{"logs fail closed", failClosed, Action{Kind: ActionLogs}, true, DecisionAllowReadOnly},
		{"metrics fail closed", failClosed, Action{Kind: ActionMetrics}, true, DecisionAllowReadOnly},
		{"stop protected", identified, Action{Kind: ActionContainerStop, ContainerID: "agent"}, false, DecisionDenyProtectedContainer},
		{"restart protected sibling", identified, Action{Kind: ActionContainerRestart, ContainerID: "agent-sidecar"}, false, DecisionDenyProtectedContainer},
		{"remove unprotected", identified, Action{Kind: ActionContainerRemove, ContainerID: "workload"}, true, DecisionAllowUnprotected},
		{"compose down protected", identified, Action{Kind: ActionComposeDown, ComposeProject: "dockpilot"}, false, DecisionDenyProtectedProject},
		{"compose mutation unprotected", identified, Action{Kind: ActionComposeMutation, ComposeProject: "workload"}, true, DecisionAllowUnprotected},
		{"mutation protected container", identified, Action{Kind: ActionMutation, ContainerID: "agent"}, false, DecisionDenyProtectedContainer},
		{"mutation protected project", identified, Action{Kind: ActionMutation, ComposeProject: "dockpilot"}, false, DecisionDenyProtectedProject},
		{"mutation fail closed", failClosed, Action{Kind: ActionMutation, ContainerID: "workload"}, false, DecisionDenyFailClosed},
		{"stop fail closed", failClosed, Action{Kind: ActionContainerStop, ContainerID: "workload"}, false, DecisionDenyFailClosed},
		{"missing target", identified, Action{Kind: ActionContainerRemove}, false, DecisionDenyTargetRequired},
		{"unknown action", identified, Action{Kind: ActionKind("NEW_ACTION")}, false, DecisionDenyUnknownAction},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Decide(tt.identity, tt.action)
			if got.Allowed != tt.allowed || got.Code != tt.code || got.Reason == "" {
				t.Fatalf("decision = %#v", got)
			}
		})
	}
}
