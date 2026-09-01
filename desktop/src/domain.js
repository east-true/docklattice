export const PROJECT_ACTIONS = [
  "compose.pull",
  "compose.up",
  "compose.down",
  "compose.start",
  "compose.stop",
];

export const SERVICE_ACTIONS = ["compose.start", "compose.stop"];

export const DESTRUCTIVE_ACTIONS = new Set([
  "compose.pull",
  "compose.up",
  "compose.down",
]);

export const TERMINAL_OPERATION_STATES = new Set([
  "success",
  "failed",
  "rejected",
  "canceled",
  "interrupted",
]);

export function actionLabel(kind) {
  const action = String(kind || "")
    .split(".")
    .pop();
  return action ? `${action[0].toUpperCase()}${action.slice(1)}` : "Action";
}

export function hostForProject(dashboard, project) {
  return (dashboard?.hosts || []).find((host) => host.id === project.agent_id);
}

export function projectMutation(project, host) {
  if (!host?.capabilities?.connection?.enabled) {
    return {
      available: false,
      reason:
        host?.capabilities?.connection?.reason || "The Agent is unavailable.",
    };
  }
  if (!host?.capabilities?.compose?.enabled) {
    return {
      available: false,
      reason:
        host?.capabilities?.compose?.reason ||
        "Docker Compose operations are unavailable.",
    };
  }
  if (!project.compose_executable) {
    return { available: false, reason: "Docker Compose is unavailable." };
  }
  if (project.restore_recovery_required) {
    return { available: false, reason: "Restore recovery is required." };
  }
  if (project.read_only) {
    return { available: false, reason: "The Compose project is read-only." };
  }
  return { available: true, reason: "" };
}

export function projectActionAvailability(project, host, kind) {
  const mutation = projectMutation(project, host);
  if (!mutation.available) return mutation;

  if (kind === "compose.pull" && !(project.pull_services || []).length) {
    return {
      available: false,
      reason: "No Service declares an Image that this widget can pull.",
    };
  }
  if (kind === "compose.up" && !project.project_up_available) {
    return {
      available: false,
      reason:
        project.project_up_reason ||
        "The effective Compose model requires an Image build.",
    };
  }
  return mutation;
}

export function serviceRuntime(runtime, serviceName) {
  return (runtime?.services || []).find(
    (service) => service.name === serviceName,
  );
}

export function serviceRuntimeLabel(runtimeService) {
  const states = [
    ...new Set(
      (runtimeService?.containers || [])
        .filter((container) => !container.one_off)
        .map((container) => String(container.state || "").toLowerCase())
        .filter(Boolean),
    ),
  ];
  if (states.length === 1) return states[0];
  if (states.length > 1) return "mixed";
  if (runtimeService?.profile_inactive) return "excluded by profile";
  return runtimeService?.status || "no container";
}

export function serviceActionAvailability(
  project,
  host,
  service,
  runtime,
  kind,
) {
  const mutation = projectMutation(project, host);
  if (!mutation.available) return mutation;
  if (runtime?.unavailable) {
    return {
      available: false,
      reason: "Current Container state is unavailable.",
    };
  }

  const observed = serviceRuntime(runtime, service.name);
  const hasContainer = (observed?.containers || []).some(
    (container) => !container.one_off,
  );
  return hasContainer
    ? mutation
    : {
        available: false,
        reason: "No existing Container was observed for this Service.",
      };
}

export function isOperationTerminal(operation) {
  return TERMINAL_OPERATION_STATES.has(
    String(operation?.status || "").toLowerCase(),
  );
}

export function operationTone(operation) {
  const status = String(operation?.status || "").toLowerCase();
  if (status === "success" && !operation?.partial_effects_possible) {
    return "success";
  }
  if (status === "failed" || status === "rejected") return "error";
  if (
    status === "canceled" ||
    status === "interrupted" ||
    operation?.partial_effects_possible
  ) {
    return "warning";
  }
  return "info";
}

export function sortProjects(projects) {
  return [...(projects || [])].sort((left, right) =>
    String(left.name || left.uid).localeCompare(
      String(right.name || right.uid),
    ),
  );
}
