import assert from "node:assert/strict";
import test from "node:test";

import {
  PROJECT_ACTIONS,
  SERVICE_ACTIONS,
  isOperationTerminal,
  operationTone,
  projectActionAvailability,
  serviceActionAvailability,
  serviceRuntimeLabel,
} from "../src/domain.js";

const host = {
  id: "agent-a",
  capabilities: {
    connection: { enabled: true },
    compose: { enabled: true },
  },
};

const project = {
  uid: "project-a",
  agent_id: "agent-a",
  compose_executable: true,
  read_only: false,
  restore_recovery_required: false,
  pull_services: ["api"],
  project_up_available: true,
};

test("the widget exposes only the approved five project actions", () => {
  assert.deepEqual(PROJECT_ACTIONS, [
    "compose.pull",
    "compose.up",
    "compose.down",
    "compose.start",
    "compose.stop",
  ]);
  assert.deepEqual(SERVICE_ACTIONS, ["compose.start", "compose.stop"]);
  assert.ok(!PROJECT_ACTIONS.includes("compose.restart"));
});

test("project mutations respect connection and Compose capability reasons", () => {
  const offline = structuredClone(host);
  offline.capabilities.connection = {
    enabled: false,
    reason: "Agent offline",
  };
  assert.deepEqual(
    projectActionAvailability(project, offline, "compose.start"),
    { available: false, reason: "Agent offline" },
  );

  const noCompose = structuredClone(host);
  noCompose.capabilities.compose = {
    enabled: false,
    reason: "Compose missing",
  };
  assert.deepEqual(
    projectActionAvailability(project, noCompose, "compose.start"),
    { available: false, reason: "Compose missing" },
  );
});

test("Pull and Up preserve the no-build availability contract", () => {
  assert.equal(
    projectActionAvailability(project, host, "compose.pull").available,
    true,
  );
  assert.equal(
    projectActionAvailability(project, host, "compose.up").available,
    true,
  );

  const blocked = {
    ...project,
    pull_services: [],
    project_up_available: false,
    project_up_reason: "worker requires an Image build",
  };
  assert.match(
    projectActionAvailability(blocked, host, "compose.pull").reason,
    /no Service declares an Image/i,
  );
  assert.equal(
    projectActionAvailability(blocked, host, "compose.up").reason,
    "worker requires an Image build",
  );
});

test("Service Start and Stop require an observed non-one-off Container", () => {
  const service = { name: "api" };
  const missing = { services: [{ name: "api", containers: [] }] };
  assert.equal(
    serviceActionAvailability(project, host, service, missing, "compose.stop")
      .available,
    false,
  );

  const oneOff = {
    services: [
      { name: "api", containers: [{ state: "running", one_off: true }] },
    ],
  };
  assert.equal(
    serviceActionAvailability(project, host, service, oneOff, "compose.start")
      .available,
    false,
  );

  const running = {
    services: [
      { name: "api", containers: [{ state: "running", one_off: false }] },
    ],
  };
  assert.equal(
    serviceActionAvailability(project, host, service, running, "compose.stop")
      .available,
    true,
  );
});

test("Service summaries use observed Docker Container states", () => {
  assert.equal(
    serviceRuntimeLabel({ containers: [{ state: "running" }] }),
    "running",
  );
  assert.equal(
    serviceRuntimeLabel({
      containers: [{ state: "running" }, { state: "exited" }],
    }),
    "mixed",
  );
  assert.equal(
    serviceRuntimeLabel({ profile_inactive: true, containers: [] }),
    "excluded by profile",
  );
});

test("operation presentation distinguishes terminal and partial outcomes", () => {
  assert.equal(isOperationTerminal({ status: "success" }), true);
  assert.equal(isOperationTerminal({ status: "running" }), false);
  assert.equal(operationTone({ status: "success" }), "success");
  assert.equal(
    operationTone({ status: "success", partial_effects_possible: true }),
    "warning",
  );
  assert.equal(operationTone({ status: "failed" }), "error");
});
