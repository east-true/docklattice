import { execFile } from "node:child_process";
import { mkdir } from "node:fs/promises";
import { promisify } from "node:util";

import { expect, test } from "@playwright/test";

const execFileAsync = promisify(execFile);
const evidenceDirectory =
  process.env.DOCKPILOT_VM_EVIDENCE_DIRECTORY || "test-results/vm-acceptance";
const vmAcceptanceEnabled =
  process.env.DOCKPILOT_VM_ACCEPTANCE === "1" &&
  Boolean(process.env.PLAYWRIGHT_TEST_BASE_URL) &&
  Boolean(process.env.DOCKPILOT_VM_SSH_HOST) &&
  Boolean(process.env.DOCKPILOT_VM_SSH_KEY);
const vmSSHHost = process.env.DOCKPILOT_VM_SSH_HOST;
const vmSSHUser = process.env.DOCKPILOT_VM_SSH_USER || "dockpilot";
const vmSSHKey = process.env.DOCKPILOT_VM_SSH_KEY;
const vmSSHKnownHosts = process.env.DOCKPILOT_VM_SSH_KNOWN_HOSTS || "/dev/null";

test.skip(
  !vmAcceptanceEnabled,
  "Set the DOCKPILOT_VM_* variables to run destructive live-VM acceptance.",
);

test.describe.configure({
  mode: "serial",
});

test.use({
  ignoreHTTPSErrors: true,
  viewport: {
    width: 1440,
    height: 1000,
  },
});

test.setTimeout(180_000);

test.beforeAll(async () => {
  await mkdir(evidenceDirectory, {
    recursive: true,
  });
});

async function vmExec(command) {
  return execFileAsync(
    "ssh",
    [
      "-i",
      vmSSHKey,
      "-o",
      "StrictHostKeyChecking=no",
      "-o",
      `UserKnownHostsFile=${vmSSHKnownHosts}`,
      `${vmSSHUser}@${vmSSHHost}`,
      command,
    ],
    {
      maxBuffer: 4 * 1024 * 1024,
      timeout: 180_000,
    },
  );
}

async function recreateAgent() {
  const command = [
    "docker run --detach",
    "--name dockpilot-agent",
    "--hostname dockpilot-agent",
    "--network dockpilot-acceptance-net",
    "--restart unless-stopped",
    "--user 65532:65532",
    "--group-add 112",
    "--volume dockpilot-agent-state:/var/lib/dockpilot:z",
    "--volume /opt/dockpilot-acceptance/bootstrap/server-ca.crt:/var/lib/dockpilot/server-ca.crt:ro",
    "--volume /opt/dockpilot-acceptance/bootstrap/join-token:/var/lib/dockpilot/join-token:ro",
    "--volume /opt/dockpilot-acceptance/fixtures/stacks:/opt/dockpilot-acceptance/fixtures/stacks:rw",
    "--volume /opt/dockpilot-acceptance/fixtures/stacks-ro:/opt/dockpilot-acceptance/fixtures/stacks-ro:ro",
    "--volume /var/run/docker.sock:/var/run/docker.sock:rw",
    "dockpilot-ui-vm:agent",
    "agent",
    "--server server:8443",
    "--registration-url https://server:8080",
    "--server-ca /var/lib/dockpilot/server-ca.crt",
    "--join-token-file /var/lib/dockpilot/join-token",
    "--display-name dockpilot-vm-acceptance",
    "--self-container-name dockpilot-agent",
    "--project-root /opt/dockpilot-acceptance/fixtures/stacks",
    "--project-root /opt/dockpilot-acceptance/fixtures/stacks-ro",
  ].join(" ");

  await vmExec("docker rm --force dockpilot-agent");
  await vmExec(command);
}

async function jsonFromPage(page, path, options = {}) {
  return page.evaluate(
    async ({ requestPath, requestOptions }) => {
      const response = await fetch(requestPath, requestOptions);
      const body = await response.json();
      if (!response.ok) {
        throw new Error(
          `${requestOptions.method || "GET"} ${requestPath} failed with ` +
            `${response.status}: ${JSON.stringify(body)}`,
        );
      }
      return body;
    },
    {
      requestPath: path,
      requestOptions: options,
    },
  );
}

async function currentDashboard(page) {
  return jsonFromPage(page, "/api/v1/dashboard");
}

async function waitForTerminalOperation(page, operation) {
  const path =
    `/api/v1/agents/${encodeURIComponent(operation.agent_id)}` +
    `/operations/${encodeURIComponent(operation.operation_id)}`;

  let finalOperation;
  await expect
    .poll(
      async () => {
        finalOperation = await jsonFromPage(page, path);
        const terminalStatuses = [
          "success",
          "failed",
          "canceled",
          "interrupted",
          "rejected",
        ];
        return terminalStatuses.includes(finalOperation.status);
      },
      {
        timeout: 120_000,
        intervals: [250, 500, 1_000],
      },
    )
    .toBe(true);

  return finalOperation;
}

async function waitForOperation(page, operation, expectedStatus = "success") {
  const finalOperation = await waitForTerminalOperation(page, operation);

  expect(
    finalOperation.status,
    `${finalOperation.error || "operation failed"}\n${finalOperation.output_tail || ""}`,
  ).toBe(expectedStatus);
  return finalOperation;
}

async function expectProblem(response, status, code) {
  expect(response.status()).toBe(status);
  expect(response.headers()["cache-control"]).toContain("no-store");
  const problem = await response.json();
  expect(problem.code).toBe(code);
  expect(problem.message).toBeTruthy();
  return problem;
}

async function waitForHost(page, predicate) {
  let host;
  await expect
    .poll(
      async () => {
        const dashboard = await currentDashboard(page);
        host = dashboard.hosts.find(
          (candidate) => candidate.display_name === "dockpilot-vm-acceptance",
        );
        return Boolean(host && predicate(host));
      },
      {
        intervals: [500, 1_000, 2_000],
        timeout: 90_000,
      },
    )
    .toBe(true);
  return host;
}

function liveContext(dashboard) {
  const host = dashboard.hosts.find(
    (candidate) => candidate.display_name === "dockpilot-vm-acceptance",
  );
  const normalProject = dashboard.projects.find(
    (project) =>
      project.name === "dockpilot-acceptance-normal" &&
      project.managed &&
      project.working_dir ===
        "/opt/dockpilot-acceptance/fixtures/stacks/normal",
  );
  const buildPolicyProject = dashboard.projects.find(
    (project) =>
      project.name === "dockpilot-acceptance-build-policy" && project.managed,
  );

  expect(host).toBeTruthy();
  expect(normalProject).toBeTruthy();
  expect(buildPolicyProject).toBeTruthy();

  return {
    buildPolicyProject,
    host,
    normalProject,
  };
}

async function runProjectOperation(page, button, confirmationLabel) {
  const responsePromise = page.waitForResponse((response) => {
    const request = response.request();
    return (
      request.method() === "POST" &&
      new URL(response.url()).pathname === "/api/v1/operations"
    );
  });

  await button.click();
  if (confirmationLabel) {
    const dialog = page.locator("#confirm-dialog");
    await expect(dialog).toBeVisible();
    await dialog.getByRole("button", { name: confirmationLabel }).click();
  }

  const response = await responsePromise;
  expect(response.ok()).toBe(true);
  const operation = await response.json();
  return waitForOperation(page, operation);
}

function observeBrowserFailures(page) {
  const failures = [];

  page.on("console", (message) => {
    if (message.type() === "error") {
      const location = message.location().url || "unknown location";
      failures.push(`${location}: ${message.text()}`);
    }
  });
  page.on("pageerror", (error) => {
    failures.push(error.message);
  });
  page.on("response", (response) => {
    if (response.status() >= 400) {
      failures.push(`${response.status()} ${response.url()}`);
    }
  });

  return failures;
}

test("VM shell, policy, host details, and responsive drawer are live", async ({
  page,
}) => {
  const browserFailures = observeBrowserFailures(page);

  await page.goto("/#/home", {
    waitUntil: "domcontentloaded",
  });
  await expect(page.locator("#sidebar-summary")).toContainText("1/1 connected");

  const dashboard = await currentDashboard(page);
  const { buildPolicyProject, host } = liveContext(dashboard);

  expect(host?.state).toBe("ACTIVE");
  expect(host?.capabilities?.docker?.enabled).toBe(true);
  expect(host?.capabilities?.compose?.enabled).toBe(true);
  expect(host?.capabilities?.discovery?.enabled).toBe(true);
  expect(host?.capabilities?.metrics?.enabled).toBe(true);
  expect(host?.capabilities?.operation_recovery?.enabled).toBe(true);
  expect(buildPolicyProject).toBeTruthy();

  await page.goto(`/#/hosts/${encodeURIComponent(host.id)}/summary`);
  const managementPanel = page.locator("section.panel").filter({
    has: page.getByRole("heading", { name: "Dockpilot management" }),
  });
  await expect(managementPanel).toContainText("discovery · Available");
  await expect(managementPanel).toContainText("operation_recovery · Available");

  await page.goto(
    `/#/projects/${encodeURIComponent(buildPolicyProject.uid)}/summary`,
  );
  await expect(page.getByText("Project Up unavailable.")).toBeVisible();
  await expect(page.getByRole("button", { name: "Up Project" })).toBeDisabled();
  await expect(page.getByText("Build required", { exact: true })).toHaveCount(
    2,
  );
  await page.screenshot({
    path: `${evidenceDirectory}/build-policy-before-mutation.png`,
    fullPage: true,
  });

  await page.setViewportSize({
    width: 375,
    height: 812,
  });
  await page.goto("/#/home");
  await page.getByRole("button", { name: "Toggle navigation" }).click();
  await expect
    .poll(async () => {
      const sidebarBox = await page.locator(".sidebar").boundingBox();
      return Math.round(sidebarBox?.x ?? Number.NaN);
    })
    .toBe(0);

  const sidebarBox = await page.locator(".sidebar").boundingBox();
  expect(sidebarBox?.width).toBe(252);
  const horizontalOverflow = await page.evaluate(
    () => document.documentElement.scrollWidth - window.innerWidth,
  );
  expect(horizontalOverflow).toBeLessThanOrEqual(1);
  await page.screenshot({
    path: `${evidenceDirectory}/mobile-navigation.png`,
  });

  expect(browserFailures).toEqual([]);
});

test("VM Compose mutations use only admitted image-backed targets", async ({
  page,
}) => {
  const browserFailures = observeBrowserFailures(page);
  await page.goto("/#/home");

  const dashboard = await currentDashboard(page);
  const { buildPolicyProject, normalProject } = liveContext(dashboard);

  await page.goto(
    `/#/projects/${encodeURIComponent(buildPolicyProject.uid)}/summary`,
  );
  const mixedServiceRow = page.locator("tbody tr").filter({
    hasText: "image-and-build",
  });
  const mixedUp = await runProjectOperation(
    page,
    mixedServiceRow.getByRole("button", { name: "Up" }),
  );
  expect(mixedUp.kind).toBe("compose.up");
  expect(mixedUp.target).toBe("image-and-build");
  expect(mixedUp.output_tail).not.toContain("Building");

  await page.goto(
    `/#/projects/${encodeURIComponent(normalProject.uid)}/summary`,
  );
  const pull = await runProjectOperation(
    page,
    page.getByRole("button", { name: "Pull Images" }),
  );
  expect(pull.kind).toBe("compose.pull");

  const up = await runProjectOperation(
    page,
    page.getByRole("button", { name: "Up Project" }),
  );
  expect(up.kind).toBe("compose.up");

  const restart = await runProjectOperation(
    page,
    page.getByRole("button", { name: "Restart", exact: true }),
    "Restart",
  );
  expect(restart.kind).toBe("compose.restart");

  const stop = await runProjectOperation(
    page,
    page.getByRole("button", { name: "Stop", exact: true }),
  );
  expect(stop.kind).toBe("compose.stop");

  const start = await runProjectOperation(
    page,
    page.getByRole("button", { name: "Start", exact: true }),
  );
  expect(start.kind).toBe("compose.start");

  await page.goto(
    `/#/projects/${encodeURIComponent(normalProject.uid)}/containers`,
  );
  await expect(
    page.getByText("Profile inactive", { exact: true }),
  ).toBeVisible();

  await page.goto(
    `/#/projects/${encodeURIComponent(buildPolicyProject.uid)}/containers`,
  );
  await expect(
    page.getByText("No container", { exact: true }).first(),
  ).toBeVisible();

  await page.goto("/#/operations");
  await expect(
    page.getByText("compose.pull", { exact: true }).first(),
  ).toBeVisible();
  await expect(
    page.getByText("compose.up", { exact: true }).first(),
  ).toBeVisible();
  await page.screenshot({
    path: `${evidenceDirectory}/successful-operations.png`,
    fullPage: true,
  });

  expect(browserFailures).toEqual([]);
});

test("VM runtime distinguishes one-off and orphan Containers and exposes live Inspectors", async ({
  page,
}) => {
  const browserFailures = observeBrowserFailures(page);
  const projectDirectory = "/opt/dockpilot-acceptance/fixtures/stacks/normal";
  const composeRunner = [
    "docker run --rm",
    "--user 65532:65532",
    "--group-add 112",
    "--volume /var/run/docker.sock:/var/run/docker.sock",
    `--volume ${projectDirectory}:${projectDirectory}`,
    `--workdir ${projectDirectory}`,
    "--entrypoint docker",
    "dockpilot-ui-vm:agent",
  ].join(" ");

  await vmExec(
    [
      `${composeRunner} compose --file compose-orphan.yaml up --detach orphan`,
      "docker inspect dockpilot-acceptance-one-off >/dev/null 2>&1 || " +
        `${composeRunner} compose --file compose.yaml run --detach ` +
        "--no-deps --name dockpilot-acceptance-one-off worker",
    ].join(" && "),
  );

  await page.goto("/#/home");
  const dashboard = await currentDashboard(page);
  const { host, normalProject } = liveContext(dashboard);
  const rescanResponse = await page.request.post("/api/v1/operations", {
    data: {
      operation_id: `discovery-rescan-${crypto.randomUUID()}`,
      agent_id: host.id,
      kind: "discovery.rescan",
    },
  });
  expect(rescanResponse.ok()).toBe(true);
  await waitForOperation(page, await rescanResponse.json());
  await currentDashboard(page);

  const runtimePath = `/api/v1/projects/${encodeURIComponent(normalProject.uid)}/runtime`;

  let runtime;
  await expect
    .poll(
      async () => {
        runtime = await jsonFromPage(page, runtimePath);
        const oneOffCount = (runtime.services || []).reduce(
          (count, service) =>
            count +
            (service.containers || []).filter((container) => container.one_off)
              .length,
          0,
        );
        return {
          oneOffCount,
          orphanCount: (runtime.orphans || []).length,
        };
      },
      {
        intervals: [1_000, 2_000, 5_000],
        timeout: 90_000,
      },
    )
    .toEqual({
      oneOffCount: 1,
      orphanCount: 1,
    });

  await page.goto(
    `/#/projects/${encodeURIComponent(normalProject.uid)}/containers`,
  );
  await expect(page.getByText("One-off", { exact: true })).toBeVisible();
  await expect(page.getByText("Orphan", { exact: true })).toBeVisible();
  await expect(
    page.getByText("Profile inactive", { exact: true }),
  ).toBeVisible();

  const webService = runtime.services.find((service) => service.name === "web");
  const webContainer = webService?.containers.find(
    (container) => !container.one_off,
  );
  expect(webContainer).toBeTruthy();

  await page.goto(
    `/#/projects/${encodeURIComponent(normalProject.uid)}/containers` +
      `?inspect=${encodeURIComponent(webContainer.id)}`,
  );
  const inspector = page.locator("#inspector");
  await expect(inspector).toBeVisible();
  await expect(inspector).toContainText("Logging driver");
  await expect(inspector).toContainText("volume");
  await expect(inspector).toContainText("bind");
  await expect(inspector).toContainText("/var/lib/acceptance");
  await expect(inspector).toContainText("/etc/acceptance/settings.conf");
  await expect(inspector).toContainText("acceptance-web");

  const networkName = "dockpilot-acceptance-normal_acceptance-net";
  const networks = await jsonFromPage(
    page,
    `/api/v1/hosts/${encodeURIComponent(host.id)}/networks`,
  );
  const acceptanceNetwork = networks.find(
    (network) => network.name === networkName,
  );
  expect(acceptanceNetwork).toBeTruthy();
  await page.goto(
    `/#/hosts/${encodeURIComponent(host.id)}/networks` +
      `?inspect=${encodeURIComponent(acceptanceNetwork.id)}`,
  );
  await expect(inspector).toContainText("IPAM configuration 1");
  await expect(inspector).toContainText("IPAM configuration 2");
  await expect(inspector).toContainText("10.51.0.0/24");
  await expect(inspector).toContainText("fd00:51::/64");
  await expect(inspector).toContainText("dockpilot-acceptance-normal-web-1");

  const volumeName = "dockpilot-acceptance-normal_acceptance-data";
  await page.goto(
    `/#/hosts/${encodeURIComponent(host.id)}/volumes` +
      `?inspect=${encodeURIComponent(volumeName)}`,
  );
  await expect(inspector).toContainText("Container references");
  await expect(inspector).toContainText("dockpilot-acceptance-normal-web-1");
  await expect(inspector).not.toContainText("Size 0 B");

  const images = await jsonFromPage(
    page,
    `/api/v1/hosts/${encodeURIComponent(host.id)}/images`,
  );
  const alpine = images.find((image) =>
    (image.repo_tags || []).includes("alpine:3.22"),
  );
  expect(alpine).toBeTruthy();
  await page.goto(
    `/#/hosts/${encodeURIComponent(host.id)}/images` +
      `?inspect=${encodeURIComponent(alpine.id)}`,
  );
  await expect(inspector).toContainText("Container usage");
  await expect(inspector).toContainText("dockpilot-acceptance-normal-web-1");

  await page.goto(`/#/hosts/${encodeURIComponent(host.id)}/containers`);
  const agentRow = page.locator("tbody tr").filter({
    hasText: "dockpilot-agent",
  });
  await expect(agentRow).toContainText("Protected");
  await expect(agentRow).toContainText(
    "Container is protected as the Agent or a member of its Compose project",
  );

  await page.screenshot({
    path: `${evidenceDirectory}/live-container-inventory.png`,
    fullPage: true,
  });
  expect(browserFailures).toEqual([]);
});

test("VM Logs stream Engine output, clear only the browser, and explain unsupported drivers", async ({
  page,
}) => {
  const browserFailures = observeBrowserFailures(page);
  await page.goto("/#/home");
  const { normalProject } = liveContext(await currentDashboard(page));

  await page.goto(`/#/projects/${encodeURIComponent(normalProject.uid)}/logs`);
  const containerSelect = page.locator("#logs-container");
  const webOption = containerSelect.locator("option").filter({
    hasText: "dockpilot-acceptance-normal-web-1",
  });
  const webContainerID = await webOption.getAttribute("value");
  expect(webContainerID).toBeTruthy();
  await containerSelect.selectOption(webContainerID);
  await page.getByRole("button", { name: "Start stream" }).click();
  await expect(page.locator("#logs-output")).toContainText(
    "acceptance web log",
    {
      timeout: 30_000,
    },
  );
  await expect(page.locator("#logs-status")).toContainText(
    "Docker Engine-retained logs",
  );

  await page.getByRole("button", { name: "Clear view" }).click();
  await expect(page.locator("#logs-output")).toHaveText("");
  await expect(page.locator("#logs-status")).toHaveText(
    "Browser view cleared. Docker Engine logs were not deleted.",
  );

  const noLogOption = containerSelect.locator("option").filter({
    hasText: "dockpilot-acceptance-normal-nolog-1",
  });
  const noLogContainerID = await noLogOption.getAttribute("value");
  expect(noLogContainerID).toBeTruthy();
  await containerSelect.selectOption(noLogContainerID);
  await page.getByRole("button", { name: "Start stream" }).click();
  await expect(page.locator("#logs-status")).toContainText(/logging|driver/i, {
    timeout: 30_000,
  });

  await page.screenshot({
    path: `${evidenceDirectory}/logging-driver-unavailable.png`,
    fullPage: true,
  });
  expect(browserFailures).toEqual([]);
});

test("VM Files enforce reveal, optimistic concurrency, backup, and restore boundaries", async ({
  page,
}) => {
  const browserFailures = observeBrowserFailures(page);
  await page.goto("/#/home");
  const { normalProject } = liveContext(await currentDashboard(page));
  const projectPath = `/api/v1/projects/${encodeURIComponent(normalProject.uid)}`;

  const runtimeBefore = await jsonFromPage(page, `${projectPath}/runtime`);
  const webContainerBefore = runtimeBefore.services
    .find((service) => service.name === "web")
    ?.containers.find((container) => !container.one_off)?.id;
  expect(webContainerBefore).toBeTruthy();

  await page.goto(`/#/projects/${encodeURIComponent(normalProject.uid)}/files`);
  await page.getByRole("button", { name: "Resolved config (masked)" }).click();
  await expect(page.locator("#editor-title")).toHaveText(
    "Resolved config — masked",
  );
  await expect(page.locator("#file-editor")).not.toHaveValue(
    /fixture-secret-value/,
  );
  await expect(
    page.getByRole("button", { name: "Reveal resolved values" }),
  ).toBeVisible();

  await page.getByRole("button", { name: "Reveal resolved values" }).click();
  const revealDialog = page.locator("#confirm-dialog");
  await expect(revealDialog).toContainText(
    "Resolved configuration may contain sensitive environment values.",
  );
  await revealDialog.getByRole("button", { name: "Reveal" }).click();
  await expect(page.locator("#editor-title")).toHaveText(
    "Resolved config — revealed",
  );
  await expect(page.locator("#file-editor")).toHaveValue(
    /fixture-secret-value/,
  );

  const interpolationGroup = page.locator(".source-group").filter({
    has: page.getByRole("heading", { name: "Interpolation environment" }),
  });
  const secretsGroup = page.locator(".source-group").filter({
    has: page.getByRole("heading", { name: "Compose secrets" }),
  });
  await expect(interpolationGroup).toContainText(".env");
  await expect(secretsGroup).not.toContainText(".env");

  await page.getByRole("button", { name: ".env", exact: true }).click();
  await expect(page.locator("#file-status")).toContainText(
    "Sensitive file is masked",
  );
  await expect(
    page.getByRole("button", { name: "Reveal sensitive file" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Reveal sensitive file" }).click();
  await revealDialog.getByRole("button", { name: "Reveal" }).click();
  await expect(page.locator("#file-editor")).toHaveValue(
    /ACCEPTANCE_IMAGE_TAG=3\.22/,
  );
  await expect(page.locator("#file-editor")).toBeDisabled();

  await page.getByRole("button", { name: "service.env" }).click();
  await expect(page.locator("#file-status")).toContainText(
    "Sensitive file is masked",
  );
  await expect(page.locator("#file-editor")).not.toHaveValue(
    /fixture-secret-value/,
  );
  await page.getByRole("button", { name: "Reveal sensitive file" }).click();
  await revealDialog.getByRole("button", { name: "Reveal" }).click();
  await expect(page.locator("#file-editor")).toHaveValue(
    /fixture-secret-value/,
  );
  await expect(page.locator("#file-editor")).toBeDisabled();

  await page.getByRole("button", { name: "compose.yaml", exact: true }).click();
  const editor = page.locator("#file-editor");
  await expect(editor).toBeEnabled();
  const originalContent = await editor.inputValue();
  const savedMarker = `# vm acceptance save ${crypto.randomUUID()}`;
  await editor.fill(`${originalContent.trimEnd()}\n\n${savedMarker}\n`);

  const saveResponsePromise = page.waitForResponse((response) => {
    return (
      response.request().method() === "PUT" &&
      new URL(response.url()).pathname === `${projectPath}/files`
    );
  });
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(revealDialog).toContainText(
    "Dockpilot creates a pre-write backup and applies optimistic hash protection.",
  );
  await revealDialog.getByRole("button", { name: "Save" }).click();
  const saveResponse = await saveResponsePromise;
  expect(saveResponse.ok()).toBe(true);
  const saveOperation = await waitForOperation(page, await saveResponse.json());
  expect(saveOperation.kind).toBe("compose.file.write");

  await page.getByRole("button", { name: "compose.yaml", exact: true }).click();
  await expect.poll(async () => editor.inputValue()).toContain(savedMarker);
  const currentFileResponse = await page.request.get(
    `${projectPath}/files?path=compose.yaml`,
  );
  expect(currentFileResponse.ok()).toBe(true);
  const currentFile = await currentFileResponse.json();
  const staleMarker = `# stale write must lose ${crypto.randomUUID()}`;
  await editor.fill(`${currentFile.content.trimEnd()}\n\n${staleMarker}\n`);

  const winnerMarker = `# concurrent winner ${crypto.randomUUID()}`;
  const winnerResponse = await page.request.put(`${projectPath}/files`, {
    data: {
      operation_id: `file-winner-${crypto.randomUUID()}`,
      relative_path: "compose.yaml",
      expected_sha256: currentFile.sha256,
      content: `${currentFile.content.trimEnd()}\n\n${winnerMarker}\n`,
    },
  });
  expect(winnerResponse.ok()).toBe(true);
  await waitForOperation(page, await winnerResponse.json());

  const staleResponsePromise = page.waitForResponse((response) => {
    return (
      response.request().method() === "PUT" &&
      new URL(response.url()).pathname === `${projectPath}/files`
    );
  });
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await revealDialog.getByRole("button", { name: "Save" }).click();
  const staleResponse = await staleResponsePromise;
  expect(staleResponse.ok()).toBe(true);
  const staleOperation = await waitForOperation(
    page,
    await staleResponse.json(),
    "failed",
  );
  expect(`${staleOperation.error}\n${staleOperation.output_tail}`).toMatch(
    /conflict|changed|sha256/i,
  );

  const finalFileResponse = await page.request.get(
    `${projectPath}/files?path=compose.yaml`,
  );
  const finalFile = await finalFileResponse.json();
  expect(finalFile.content).toContain(winnerMarker);
  expect(finalFile.content).not.toContain(staleMarker);

  await page.goto(
    `/#/projects/${encodeURIComponent(normalProject.uid)}/backups`,
  );
  await expect(
    page.getByText("Restore changes configuration files only."),
  ).toBeVisible();
  const restoreButton = page
    .getByRole("button", { name: "Restore", exact: true })
    .first();
  await expect(restoreButton).toBeEnabled();
  const restoreResponsePromise = page.waitForResponse((response) => {
    return (
      response.request().method() === "POST" &&
      new URL(response.url()).pathname.endsWith("/restore")
    );
  });
  await restoreButton.click();
  await expect(revealDialog).toContainText("Restore never runs Compose Up.");
  await revealDialog
    .getByRole("button", { name: "Restore", exact: true })
    .click();
  const restoreResponse = await restoreResponsePromise;
  expect(restoreResponse.ok()).toBe(true);
  const restoreOperation = await waitForOperation(
    page,
    await restoreResponse.json(),
  );
  expect(restoreOperation.kind).toBe("backup.restore");

  const runtimeAfter = await jsonFromPage(page, `${projectPath}/runtime`);
  const webContainerAfter = runtimeAfter.services
    .find((service) => service.name === "web")
    ?.containers.find((container) => !container.one_off)?.id;
  expect(webContainerAfter).toBe(webContainerBefore);

  await page.screenshot({
    path: `${evidenceDirectory}/configuration-backups.png`,
    fullPage: true,
  });
  expect(browserFailures).toEqual([]);
});

test("VM rejects malformed, oversized, escaping, and command-shaped input", async ({
  page,
}) => {
  await page.goto("/#/home");
  const dashboard = await currentDashboard(page);
  const { host, normalProject } = liveContext(dashboard);
  const projectPath = `/api/v1/projects/${encodeURIComponent(normalProject.uid)}`;

  for (const relativePath of ["../compose.yaml", "/etc/passwd"]) {
    const response = await page.request.get(
      `${projectPath}/files?path=${encodeURIComponent(relativePath)}`,
    );
    await expectProblem(response, 400, "INVALID_REQUEST");
  }

  const unknownFile = await page.request.get(
    `${projectPath}/files?path=${encodeURIComponent("not-managed.txt")}`,
  );
  await expectProblem(unknownFile, 400, "INVALID_REQUEST");

  const unknownField = await page.request.post("/api/v1/operations", {
    data: {
      operation_id: `unknown-field-${crypto.randomUUID()}`,
      agent_id: host.id,
      kind: "discovery.rescan",
      secret: "must-not-be-accepted",
    },
  });
  await expectProblem(unknownField, 400, "INVALID_REQUEST");

  const duplicateField = await page.request.post("/api/v1/operations", {
    data:
      `{"operation_id":"duplicate-${crypto.randomUUID()}",` +
      `"agent_id":"${host.id}","kind":"discovery.rescan",` +
      '"kind":"compose.down"}',
    headers: {
      "Content-Type": "application/json",
    },
  });
  await expectProblem(duplicateField, 400, "INVALID_REQUEST");

  const currentFile = await page.request.get(
    `${projectPath}/files?path=compose.yaml`,
  );
  expect(currentFile.ok()).toBe(true);
  const currentFileBody = await currentFile.json();
  const oversizedWrite = await page.request.put(`${projectPath}/files`, {
    data: {
      operation_id: `oversized-${crypto.randomUUID()}`,
      relative_path: "compose.yaml",
      expected_sha256: currentFileBody.sha256,
      content: "x".repeat(7 * 1024 * 1024),
    },
  });
  await expectProblem(oversizedWrite, 413, "TOO_LARGE");

  const injectionMarker = "/tmp/dockpilot-command-injection-marker";
  await vmExec(`docker exec --user 0 dockpilot-agent rm -f ${injectionMarker}`);
  const commandShapedTarget = await page.request.post("/api/v1/operations", {
    data: {
      operation_id: `target-injection-${crypto.randomUUID()}`,
      agent_id: host.id,
      project_uid: normalProject.uid,
      kind: "compose.up",
      target: `web;touch ${injectionMarker}`,
    },
  });
  await expectProblem(commandShapedTarget, 409, "CONFLICT");

  const commandShapedKind = await page.request.post("/api/v1/operations", {
    data: {
      operation_id: `kind-injection-${crypto.randomUUID()}`,
      agent_id: host.id,
      kind: `discovery.rescan;touch ${injectionMarker}`,
    },
  });
  await expectProblem(commandShapedKind, 503, "CAPABILITY_UNAVAILABLE");
  await vmExec(`docker exec dockpilot-agent test ! -e ${injectionMarker}`);

  const operationID = `idempotency-${crypto.randomUUID()}`;
  const originalOperation = await page.request.post("/api/v1/operations", {
    data: {
      operation_id: operationID,
      agent_id: host.id,
      kind: "discovery.rescan",
    },
  });
  expect(originalOperation.status()).toBe(202);
  await waitForOperation(page, await originalOperation.json());

  const mismatchedReplay = await page.request.post("/api/v1/operations", {
    data: {
      operation_id: operationID,
      agent_id: host.id,
      project_uid: normalProject.uid,
      kind: "compose.stop",
    },
  });
  await expectProblem(mismatchedReplay, 409, "CONFLICT");

  const readOnlyProject = dashboard.projects.find(
    (project) => project.read_only && !project.collision,
  );
  const collisionProject = dashboard.projects.find(
    (project) => project.collision,
  );
  expect(readOnlyProject).toBeTruthy();
  expect(collisionProject).toBeTruthy();

  for (const project of [readOnlyProject, collisionProject]) {
    const response = await page.request.post("/api/v1/operations", {
      data: {
        operation_id: `immutable-project-${crypto.randomUUID()}`,
        agent_id: project.agent_id,
        project_uid: project.uid,
        kind: "compose.stop",
      },
    });
    await expectProblem(response, 409, "CONFLICT");
  }
});

test("VM project lock rejects a mutation storm and cancellation remains bounded", async ({
  page,
}) => {
  await page.goto("/#/home");
  const { host, normalProject } = liveContext(await currentDashboard(page));

  const restartResponses = await Promise.all(
    Array.from({ length: 8 }, () =>
      page.request.post("/api/v1/operations", {
        data: {
          operation_id: `restart-storm-${crypto.randomUUID()}`,
          agent_id: host.id,
          project_uid: normalProject.uid,
          kind: "compose.restart",
        },
      }),
    ),
  );

  const accepted = [];
  const refused = [];
  for (const response of restartResponses) {
    if (response.status() === 202) {
      accepted.push(await response.json());
    } else {
      refused.push(
        await expectProblem(response, 503, "CAPABILITY_UNAVAILABLE"),
      );
    }
  }

  expect(accepted.length).toBeGreaterThanOrEqual(1);
  const terminalOperations = [];
  for (const operation of accepted) {
    const finalOperation = await waitForTerminalOperation(page, operation);
    expect(["success", "failed", "rejected", "canceled"]).toContain(
      finalOperation.status,
    );
    terminalOperations.push(finalOperation);
  }

  expect(
    terminalOperations.filter((operation) => operation.status === "success")
      .length,
  ).toBeGreaterThanOrEqual(1);
  const contentionMessages = [
    ...refused.map((problem) => problem.message),
    ...terminalOperations
      .filter((operation) => operation.status !== "success")
      .map(
        (operation) =>
          `${operation.error || ""}\n${operation.output_tail || ""}`,
      ),
  ];
  expect(contentionMessages.length).toBeGreaterThanOrEqual(1);
  expect(contentionMessages.join("\n")).toMatch(/busy|locked|operation/i);

  const cancelStart = await page.request.post("/api/v1/operations", {
    data: {
      operation_id: `cancel-restart-${crypto.randomUUID()}`,
      agent_id: host.id,
      project_uid: normalProject.uid,
      kind: "compose.restart",
    },
  });
  expect(cancelStart.status()).toBe(202);
  const cancelOperation = await cancelStart.json();
  const cancelResponse = await page.request.post(
    `/api/v1/agents/${encodeURIComponent(host.id)}` +
      `/operations/${encodeURIComponent(cancelOperation.operation_id)}/cancel`,
    {
      data: {},
    },
  );
  expect(cancelResponse.ok()).toBe(true);
  const cancellation = await cancelResponse.json();
  expect(["ACCEPTED", "TOO_LATE", "ALREADY_TERMINAL"]).toContain(
    cancellation.outcome,
  );

  const canceledFinal = await waitForTerminalOperation(page, cancelOperation);
  expect(["success", "failed", "canceled"]).toContain(canceledFinal.status);
  if (cancellation.outcome === "ACCEPTED") {
    expect(canceledFinal.status).toBe("canceled");
    expect(canceledFinal.partial_effects_possible).toBe(true);
  }
});

test("VM Live Metrics survives rapid route churn and confirmation focus is contained", async ({
  page,
}) => {
  const browserFailures = observeBrowserFailures(page);
  await page.goto("/#/home");
  const { host, normalProject } = liveContext(await currentDashboard(page));

  await page.goto(`/#/hosts/${encodeURIComponent(host.id)}/metrics`);
  await expect(page.locator("#metrics-status")).toContainText("Observed", {
    timeout: 30_000,
  });
  await expect(page.locator("#metrics-table tbody tr").first()).toContainText(
    "Docker workload",
  );
  await expect(page.locator("#metrics-table")).toContainText(
    "dockpilot-acceptance-normal",
  );
  await page.getByRole("button", { name: "Top Containers" }).click();
  await expect(
    page.getByRole("button", { name: "Top Containers" }),
  ).toHaveClass(/active/);

  await page.goto(
    `/#/projects/${encodeURIComponent(normalProject.uid)}/summary`,
  );
  const downButton = page.getByRole("button", {
    name: "Down",
    exact: true,
  });
  await downButton.focus();
  await downButton.click();
  const dialog = page.locator("#confirm-dialog");
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText(
    "Named Volumes and external Networks or Volumes will be retained.",
  );
  await expect(dialog.getByRole("button", { name: "Cancel" })).toBeFocused();
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(dialog).not.toBeVisible();
  await expect(downButton).toBeFocused();

  const routes = [
    `#/projects/${encodeURIComponent(normalProject.uid)}/logs`,
    `#/hosts/${encodeURIComponent(host.id)}/metrics`,
    "#/operations",
    `#/projects/${encodeURIComponent(normalProject.uid)}/containers`,
    `#/hosts/${encodeURIComponent(host.id)}/summary`,
  ];
  await page.evaluate(async (rapidRoutes) => {
    for (const route of rapidRoutes) {
      window.location.hash = route;
      await new Promise((resolve) => window.setTimeout(resolve, 25));
    }
  }, routes);
  await expect(
    page.getByRole("heading", { name: "Dockpilot management" }),
  ).toBeVisible();
  expect(browserFailures).toEqual([]);
});

test("VM reports Agent, Compose, and Docker failures and recovers each dependency", async ({
  page,
}) => {
  await page.goto("/#/home");
  const { normalProject } = liveContext(await currentDashboard(page));
  const composePlugin = "/usr/local/libexec/docker/cli-plugins/docker-compose";
  const disabledComposePlugin = `${composePlugin}.disabled-by-acceptance`;

  try {
    await vmExec("docker stop dockpilot-agent");
    const offlineHost = await waitForHost(
      page,
      (candidate) => candidate.state !== "ACTIVE",
    );
    expect(offlineHost.capabilities.connection.enabled).toBe(false);
    await page.reload();
    await expect(page.locator("#sidebar-summary")).toContainText(
      "0/1 connected",
    );
  } finally {
    await vmExec("docker start dockpilot-agent");
    await waitForHost(
      page,
      (candidate) =>
        candidate.state === "ACTIVE" &&
        candidate.capabilities.connection.enabled,
    );
  }

  try {
    await vmExec(
      `docker exec --user 0 dockpilot-agent mv ${composePlugin} ${disabledComposePlugin}`,
    );
    await vmExec("docker restart dockpilot-agent");
    const composeFailure = await waitForHost(
      page,
      (candidate) => candidate.state !== "ACTIVE",
    );
    expect(composeFailure.capabilities.connection.enabled).toBe(false);
    await expect
      .poll(async () => {
        const result = await vmExec(
          "sudo docker inspect --format '{{.State.Status}}' dockpilot-agent",
        );
        return result.stdout.trim();
      })
      .toMatch(/restarting|exited/);
    await page.goto(
      `/#/projects/${encodeURIComponent(normalProject.uid)}/summary`,
    );
    await expect(
      page.getByRole("button", { name: "Up Project" }),
    ).toBeDisabled();
    await expect(
      page.getByRole("button", { name: "Restart", exact: true }),
    ).toBeDisabled();
  } finally {
    await recreateAgent();
    await waitForHost(
      page,
      (candidate) =>
        candidate.state === "ACTIVE" && candidate.capabilities.compose.enabled,
    );
  }

  try {
    await vmExec("sudo chmod 000 /var/run/docker.sock");
    await vmExec("sudo docker restart dockpilot-agent");
    const dockerFailure = await waitForHost(
      page,
      (candidate) => candidate.state !== "ACTIVE",
    );
    expect(dockerFailure.capabilities.connection.enabled).toBe(false);
    await expect
      .poll(async () => {
        const result = await vmExec(
          "sudo docker inspect --format '{{.State.Status}}' dockpilot-agent",
        );
        return result.stdout.trim();
      })
      .toMatch(/restarting|exited/);
    await page.goto(
      `/#/projects/${encodeURIComponent(normalProject.uid)}/summary`,
    );
    await expect(
      page.getByRole("button", { name: "Up Project" }),
    ).toBeDisabled();
  } finally {
    await vmExec("sudo chmod 660 /var/run/docker.sock");
    await recreateAgent();
    await waitForHost(
      page,
      (candidate) =>
        candidate.state === "ACTIVE" &&
        candidate.capabilities.docker.enabled &&
        candidate.capabilities.compose.enabled &&
        candidate.capabilities.metrics.enabled,
    );
  }
});

test("VM recovers an operation record across Server and Agent restart", async ({
  page,
}) => {
  await page.goto("/#/home");
  const { host, normalProject } = liveContext(await currentDashboard(page));
  const response = await page.request.post("/api/v1/operations", {
    data: {
      operation_id: `restart-recovery-${crypto.randomUUID()}`,
      agent_id: host.id,
      project_uid: normalProject.uid,
      kind: "compose.restart",
    },
  });
  expect(response.status()).toBe(202);
  const operation = await response.json();

  await vmExec("docker restart dockpilot-server");
  await expect
    .poll(
      async () => {
        try {
          const readiness = await page.request.get("/api/v1/dashboard");
          return readiness.ok();
        } catch {
          return false;
        }
      },
      {
        intervals: [500, 1_000, 2_000],
        timeout: 100_000,
      },
    )
    .toBe(true);

  await vmExec("docker restart dockpilot-agent");
  await waitForHost(page, (candidate) => candidate.state === "ACTIVE");
  const recovered = await waitForTerminalOperation(page, operation);
  expect(recovered.operation_id).toBe(operation.operation_id);
  expect(["success", "canceled", "interrupted", "failed"]).toContain(
    recovered.status,
  );
});

test("VM Compose Down removes runtime objects, retains data, and Up restores service", async ({
  page,
}) => {
  const browserFailures = observeBrowserFailures(page);
  await page.goto("/#/home");
  const { host, normalProject } = liveContext(await currentDashboard(page));
  const volumeName = "dockpilot-acceptance-normal_acceptance-data";
  const networkName = "dockpilot-acceptance-normal_acceptance-net";
  const orphanNetworkName = "dockpilot-acceptance-normal_default";
  const volumeBefore = await vmExec(
    `docker volume inspect --format '{{.Mountpoint}}' ${volumeName}`,
  );

  await page.goto(
    `/#/projects/${encodeURIComponent(normalProject.uid)}/summary`,
  );
  const down = await runProjectOperation(
    page,
    page.getByRole("button", { name: "Down", exact: true }),
    "Down",
  );
  expect(down.kind).toBe("compose.down");

  const remaining = await vmExec(
    "docker ps --all " +
      "--filter label=com.docker.compose.project=dockpilot-acceptance-normal " +
      "--format '{{.Names}}'",
  );
  const remainingNames = remaining.stdout.trim().split("\n").sort();
  expect(remainingNames).toEqual(
    [
      "dockpilot-acceptance-normal-orphan-1",
      "dockpilot-acceptance-one-off",
    ].sort(),
  );
  await vmExec(`docker volume inspect ${volumeName}`);
  const retainedNetwork = await vmExec(`docker network inspect ${networkName}`);
  expect(retainedNetwork.stdout).toContain("dockpilot-acceptance-one-off");
  const retainedOrphanNetwork = await vmExec(
    `docker network inspect ${orphanNetworkName}`,
  );
  expect(retainedOrphanNetwork.stdout).toContain(
    "dockpilot-acceptance-normal-orphan-1",
  );

  const up = await runProjectOperation(
    page,
    page.getByRole("button", { name: "Up Project" }),
  );
  expect(up.kind).toBe("compose.up");
  expect(up.output_tail).not.toContain("Building");

  const rescanResponse = await page.request.post("/api/v1/operations", {
    data: {
      operation_id: `post-up-rescan-${crypto.randomUUID()}`,
      agent_id: host.id,
      kind: "discovery.rescan",
    },
  });
  expect(rescanResponse.status()).toBe(202);
  await waitForOperation(page, await rescanResponse.json());
  await currentDashboard(page);

  let runtime;
  await expect
    .poll(
      async () => {
        runtime = await jsonFromPage(
          page,
          `/api/v1/projects/${encodeURIComponent(normalProject.uid)}/runtime`,
        );
        return runtime.services
          .filter((service) =>
            ["web", "worker", "nolog"].includes(service.name),
          )
          .every((service) =>
            service.containers.some(
              (container) =>
                !container.one_off && container.state === "running",
            ),
          );
      },
      {
        intervals: [500, 1_000, 2_000],
        timeout: 60_000,
      },
    )
    .toBe(true);

  const volumeAfter = await vmExec(
    `docker volume inspect --format '{{.Mountpoint}}' ${volumeName}`,
  );
  expect(volumeAfter.stdout.trim()).toBe(volumeBefore.stdout.trim());
  await vmExec(`docker network inspect ${networkName}`);
  await page.screenshot({
    path: `${evidenceDirectory}/destructive-down-recovered.png`,
    fullPage: true,
  });
  expect(browserFailures).toEqual([]);
});
