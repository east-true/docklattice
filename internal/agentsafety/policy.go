package agentsafety

// ActionKind identifies operations relevant to Agent self-protection.
type ActionKind string

const (
	ActionQuery            ActionKind = "QUERY"
	ActionLogs             ActionKind = "LOGS"
	ActionMetrics          ActionKind = "METRICS"
	ActionContainerStart   ActionKind = "CONTAINER_START"
	ActionContainerStop    ActionKind = "CONTAINER_STOP"
	ActionContainerRestart ActionKind = "CONTAINER_RESTART"
	ActionContainerRemove  ActionKind = "CONTAINER_REMOVE"
	ActionComposeDown      ActionKind = "COMPOSE_DOWN"
	ActionComposeMutation  ActionKind = "COMPOSE_MUTATION"
	ActionMutation         ActionKind = "MUTATION"
)

// Action describes the intended mutation target. Irrelevant target fields may
// be empty for read-only actions.
type Action struct {
	Kind           ActionKind
	ContainerID    string
	ComposeProject string
}

// DecisionCode is a stable policy result suitable for API error mapping.
type DecisionCode string

const (
	DecisionAllowReadOnly          DecisionCode = "ALLOW_READ_ONLY"
	DecisionAllowUnprotected       DecisionCode = "ALLOW_UNPROTECTED"
	DecisionDenyFailClosed         DecisionCode = "DENY_FAIL_CLOSED"
	DecisionDenyProtectedContainer DecisionCode = "DENY_PROTECTED_CONTAINER"
	DecisionDenyProtectedProject   DecisionCode = "DENY_PROTECTED_PROJECT"
	DecisionDenyTargetRequired     DecisionCode = "DENY_TARGET_REQUIRED"
	DecisionDenyUnknownAction      DecisionCode = "DENY_UNKNOWN_ACTION"
)

// Decision is the typed result consumed by Docker/Compose adapters.
type Decision struct {
	Allowed bool
	Code    DecisionCode
	Reason  string
}

// Decide applies self-protection to an action. Identification failure allows
// only query, log, and metrics traffic; every mutation fails closed.
func Decide(identity Identification, action Action) Decision {
	switch action.Kind {
	case ActionQuery, ActionLogs, ActionMetrics:
		return Decision{Allowed: true, Code: DecisionAllowReadOnly, Reason: "read-only action is always allowed"}
	case ActionContainerStart:
		if identity.FailClosed {
			return failClosedDecision()
		}
		if action.ContainerID == "" {
			return targetRequiredDecision()
		}
		// Starting the protected Agent cannot stop or remove it, but remains a
		// mutation and therefore still fails closed when identity is unknown.
		return Decision{Allowed: true, Code: DecisionAllowUnprotected, Reason: "starting an identified container does not endanger the Agent"}
	case ActionContainerStop, ActionContainerRestart, ActionContainerRemove:
		if identity.FailClosed {
			return failClosedDecision()
		}
		if action.ContainerID == "" {
			return targetRequiredDecision()
		}
		if contains(identity.ProtectedContainerIDs, action.ContainerID) {
			return Decision{Code: DecisionDenyProtectedContainer, Reason: "target container is protected as an Agent or member of its Compose project"}
		}
	case ActionComposeDown, ActionComposeMutation:
		if identity.FailClosed {
			return failClosedDecision()
		}
		if action.ComposeProject == "" {
			return targetRequiredDecision()
		}
		if contains(identity.ProtectedComposeProjects, action.ComposeProject) {
			return Decision{Code: DecisionDenyProtectedProject, Reason: "target Compose project contains a protected Agent"}
		}
	case ActionMutation:
		if identity.FailClosed {
			return failClosedDecision()
		}
		if action.ContainerID == "" && action.ComposeProject == "" {
			return targetRequiredDecision()
		}
		if contains(identity.ProtectedContainerIDs, action.ContainerID) {
			return Decision{Code: DecisionDenyProtectedContainer, Reason: "mutation targets a protected Agent container"}
		}
		if contains(identity.ProtectedComposeProjects, action.ComposeProject) {
			return Decision{Code: DecisionDenyProtectedProject, Reason: "mutation targets a protected Agent Compose project"}
		}
	default:
		return Decision{Code: DecisionDenyUnknownAction, Reason: "unknown action is denied"}
	}
	return Decision{Allowed: true, Code: DecisionAllowUnprotected, Reason: "mutation target is outside the Agent protection set"}
}

func failClosedDecision() Decision {
	return Decision{Code: DecisionDenyFailClosed, Reason: "Agent identity is unknown; only query, logs, and metrics are allowed"}
}

func targetRequiredDecision() Decision {
	return Decision{Code: DecisionDenyTargetRequired, Reason: "a mutation target is required"}
}

func contains(values []string, want string) bool {
	if want == "" {
		return false
	}
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
