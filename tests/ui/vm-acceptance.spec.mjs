import { execFile } from "node:child_process";
import { mkdir } from "node:fs/promises";
import { promisify } from "node:util";

import { expect, test } from "@playwright/test";

const execFileAsync = promisify(execFile);
const evidenceDirectory =
  process.env.DOCKLATTICE_VM_EVIDENCE_DIRECTORY || "test-results/vm-acceptance";
const vmAcceptanceEnabled =
  process.env.DOCKLATTICE_VM_ACCEPTANCE === "1" &&
  Boolean(process.env.PLAYWRIGHT_TEST_BASE_URL) &&
  Boolean(process.env.DOCKLATTICE_VM_SSH_HOST) &&
  Boolean(process.env.DOCKLATTICE_VM_SSH_KEY);
const vmSSHHost = process.env.DOCKLATTICE_VM_SSH_HOST;
const vmSSHUser = process.env.DOCKLATTICE_VM_SSH_USER || "docklattice";
const vmSSHKey = process.env.DOCKLATTICE_VM_SSH_KEY;
const vmSSHKnownHosts =
  process.env.DOCKLATTICE_VM_SSH_KNOWN_HOSTS || "/dev/null";
const vmDockerSocketGID = process.env.DOCKLATTICE_VM_DOCKER_SOCKET_GID || "112";

test.skip(
  !vmAcceptanceEnabled,
  "Set the DOCKLATTICE_VM_* variables to run destructive live-VM acceptance.",
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
    "--name docklattice-agent",
    "--hostname docklattice-agent",
    "--network docklattice-acceptance-net",
    "--restart unless-stopped",
    "--user 65532:65532",
    `--group-add ${vmDockerSocketGID}`,
    "--volume docklattice-agent-state:/var/lib/docklattice:z",
    "--volume /opt/docklattice-acceptance/bootstrap/server-ca.crt:/var/lib/docklattice/server-ca.crt:ro",
    "--volume /opt/docklattice-acceptance/bootstrap/join-token:/var/lib/docklattice/join-token:ro",
    "--volume /opt/docklattice-acceptance/fixtures/stacks:/opt/docklattice-acceptance/fixtures/stacks:rw",
    "--volume /opt/docklattice-acceptance/fixtures/stacks-ro:/opt/docklattice-acceptance/fixtures/stacks-ro:ro",
    "--volume /var/run/docker.sock:/var/run/docker.sock:rw",
    "docklattice-ui-vm:agent",
    "agent",
    "--server server:8443",
    "--registration-url https://server:8080",
    "--server-ca /var/lib/docklattice/server-ca.crt",
    "--join-token-file /var/lib/docklattice/join-token",
    "--display-name docklattice-vm-acceptance",
    "--self-container-name docklattice-agent",
    "--project-root /opt/docklattice-acceptance/fixtures/stacks",
    "--project-root /opt/docklattice-acceptance/fixtures/stacks-ro",
  ].join(" ");

  await vmExec("docker rm --force docklattice-agent");
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

function withoutVMFileTestMarkers(content) {
  const testMarkerPrefixes = [
    "# vm acceptance save ",
    "# concurrent winner ",
    "# stale write must lose ",
  ];
  const retainedLines = String(content)
    .split("\n")
    .filter((line) => {
      return !testMarkerPrefixes.some((prefix) => line.startsWith(prefix));
    });

  return `${retainedLines.join("\n").trimEnd()}\n`;
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
          (candidate) => candidate.display_name === "docklattice-vm-acceptance",
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
    (candidate) => candidate.display_name === "docklattice-vm-acceptance",
  );
  const normalProject = dashboard.projects.find(
    (project) =>
      project.name === "docklattice-acceptance-normal" &&
      project.managed &&
      project.working_dir ===
        "/opt/docklattice-acceptance/fixtures/stacks/normal",
  );
  const buildPolicyProject = dashboard.projects.find(
    (project) =>
      project.name === "docklattice-acceptance-build-policy" && project.managed,
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

async function runProjectOperation(
  page,
  button,
  confirmationLabel,
  confirmationCopy,
) {
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
    if (confirmationCopy) {
      await expect(dialog).toContainText(confirmationCopy);
    }
    await dialog.getByRole("button", { name: confirmationLabel }).click();
  }

  const response = await responsePromise;
  expect(response.ok()).toBe(true);
  const operation = await response.json();
  const finalOperation = await waitForOperation(page, operation);
  await page.locator(".toast-close").evaluateAll((buttons) => {
    buttons.forEach((button) => button.click());
  });
  return finalOperation;
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
  await waitForHost(
    page,
    (candidate) =>
      candidate.capabilities?.connection?.enabled &&
      candidate.capabilities?.docker?.enabled,
  );
  await page.reload({
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
  await expect(
    page.getByRole("heading", {
      name: "Host",
      exact: true,
    }),
  ).toBeVisible();
  const topLevelPanels = await page
    .locator("#view > section.panel > .panel-header h2")
    .allTextContents();
  expect(topLevelPanels).toEqual(["Host", "Docker Engine", "Compose projects"]);

  const hostPanel = page.locator("section.host-summary-panel");
  const enginePanel = page.locator("section.engine-summary-panel");
  const technicalPanel = enginePanel.locator(".engine-technical-section");
  const cpuUsageRow = enginePanel
    .locator("dt")
    .filter({
      hasText: /^CPU used \/ total$/,
    })
    .locator("..");
  await expect(cpuUsageRow.locator("dd")).toHaveText(
    /^(?:Partial · )?\d+\.\d{2} \/ \d+ logical CPUs$/,
  );
  const memoryUsageRow = enginePanel
    .locator("dt")
    .filter({
      hasText: /^Memory used \/ total$/,
    })
    .locator("..");
  await expect(memoryUsageRow.locator("dd")).toHaveText(
    /^(?:Partial · )?\d+(?:\.\d+)? (?:B|KiB|MiB|GiB|TiB) \/ \d+(?:\.\d+)? (?:KiB|MiB|GiB|TiB) \(\d+(?:\.\d+)?%\)$/,
  );
  const observedUsageRow = enginePanel
    .locator("dt")
    .filter({
      hasText: /^Stats observed$/,
    })
    .locator("..");
  await expect(observedUsageRow.locator("dd")).not.toHaveText(
    /Loading|Unavailable/,
  );
  await expect(
    technicalPanel.getByText("Engine API version", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(
    technicalPanel.locator("dt").filter({
      hasText: /^Engine version$|^Storage driver$/,
    }),
  ).toHaveCount(0);
  const technicalColumnCount = await technicalPanel
    .locator(".definition-list")
    .evaluate((element) => {
      return getComputedStyle(element).gridTemplateColumns.split(" ").length;
    });
  expect(technicalColumnCount).toBe(2);
  const wrappedTechnicalLabels = await technicalPanel
    .locator("dt")
    .evaluateAll((elements) => {
      return elements
        .filter((element) => {
          const textRange = document.createRange();
          textRange.selectNodeContents(element);
          return textRange.getClientRects().length > 1;
        })
        .map((element) => element.textContent.trim());
    });
  expect(wrappedTechnicalLabels).toEqual([]);
  expect(
    await technicalPanel.evaluate((element) => {
      return getComputedStyle(element).borderTopWidth;
    }),
  ).toBe("0px");

  await expect(hostPanel).toContainText("Compose discovery · Available");
  await expect(hostPanel).toContainText("Operation recovery · Available");
  await expect(hostPanel).not.toContainText("operation_recovery");
  await expect(hostPanel).not.toContainText("fs_read");
  const metadataPosition = await hostPanel
    .getByText("Session source IP", {
      exact: true,
    })
    .boundingBox();
  const capabilitiesPosition = await hostPanel
    .getByRole("heading", {
      name: "Capabilities",
      exact: true,
    })
    .boundingBox();
  expect(metadataPosition).not.toBeNull();
  expect(capabilitiesPosition).not.toBeNull();
  expect(metadataPosition.y).toBeLessThan(capabilitiesPosition.y);
  expect(
    await hostPanel.locator(".management-capabilities").evaluate((element) => {
      return getComputedStyle(element).borderTopWidth;
    }),
  ).toBe("0px");
  const capabilityColumnCount = await hostPanel
    .locator(".capability-grid")
    .evaluate((element) => {
      return getComputedStyle(element).gridTemplateColumns.split(" ").length;
    });
  expect(capabilityColumnCount).toBe(4);

  await page.screenshot({
    path: `${evidenceDirectory}/host-summary.png`,
    fullPage: true,
  });

  await page.goto(`/#/hosts/${encodeURIComponent(host.id)}/compose`);
  await expect(
    page.getByRole("heading", {
      name: host.display_name || host.id,
      exact: true,
      level: 1,
    }),
  ).toBeVisible();
  await expect(
    page.getByRole("heading", {
      name: "Compose",
      exact: true,
      level: 1,
    }),
  ).toHaveCount(0);
  const composeTable = page.locator("section.panel.flush table");
  await expect(composeTable.getByRole("columnheader")).toHaveText([
    "Project",
    "Services",
    "Containers",
    "Last observed",
    "Config drift",
    "Needs attention",
  ]);
  const containerCells = await composeTable
    .locator("tbody tr td:nth-child(3)")
    .allTextContents();
  expect(containerCells.length).toBeGreaterThan(0);
  expect(
    containerCells.every((value) => /^(?:\d+|Unavailable)$/.test(value.trim())),
  ).toBe(true);
  const observationCells = composeTable.locator("tbody tr td:nth-child(4)");
  await expect(observationCells).toHaveCount(containerCells.length);
  const observationValues = await observationCells.evaluateAll((cells) => {
    return cells.map((cell) => ({
      datetime: cell.querySelector("time")?.getAttribute("datetime") || "",
      text: cell.textContent.trim(),
    }));
  });
  expect(
    observationValues.every(
      (value) => value.datetime || value.text === "Never",
    ),
  ).toBe(true);
  await expect(
    composeTable.getByText("Last known", { exact: false }),
  ).toHaveCount(0);
  const resizeHandles = composeTable.getByRole("separator", {
    name: /^Resize .+ column$/,
  });
  await expect(resizeHandles).toHaveCount(5);
  const containersResizeHandle = composeTable.getByRole("separator", {
    name: "Resize Containers column",
  });
  const containersHeading = containersResizeHandle.locator("..");
  const initialContainerColumnWidth = await containersHeading.evaluate(
    (element) => element.getBoundingClientRect().width,
  );
  await containersResizeHandle.focus();
  await page.keyboard.press("ArrowRight");
  const resizedContainerColumnWidth = await containersHeading.evaluate(
    (element) => element.getBoundingClientRect().width,
  );
  expect(resizedContainerColumnWidth).toBeGreaterThan(
    initialContainerColumnWidth,
  );
  const tableOverflow = await composeTable.evaluate((element) => {
    const wrapper = element.closest(".table-wrap");
    return wrapper.scrollWidth - wrapper.clientWidth;
  });
  expect(tableOverflow).toBeLessThanOrEqual(1);
  await page.screenshot({
    path: `${evidenceDirectory}/compose-projects.png`,
    fullPage: true,
  });

  await page.goto(
    `/#/projects/${encodeURIComponent(buildPolicyProject.uid)}/summary`,
  );
  await expect(
    page.getByRole("heading", {
      name: "Project",
      exact: true,
      level: 2,
    }),
  ).toBeVisible();
  const projectHeaderActions = await page
    .locator(".page-header .page-actions button")
    .allTextContents();
  expect(projectHeaderActions).toEqual([
    "Pull",
    "Up",
    "Down",
    "Start",
    "Stop",
    "Restart",
  ]);
  const projectSummaryPanels = await page
    .locator("#view > section.panel > .panel-header h2")
    .allTextContents();
  expect(projectSummaryPanels).toEqual([
    "Project",
    "Containers",
    "Services needing attention",
  ]);
  const projectPanel = page.locator("section.project-summary-panel");
  const runtimePanel = page.locator("section.project-runtime-panel");
  await expect(
    projectPanel.getByRole("heading", {
      name: "DockLattice management",
      exact: true,
    }),
  ).toBeVisible();
  await expect(projectPanel.getByText("Project directory")).toBeVisible();
  await expect(runtimePanel.getByText("Services in model")).toBeVisible();
  await expect(
    runtimePanel.getByRole("term").filter({ hasText: /^Containers$/ }),
  ).toBeVisible();
  const attentionPanel = page.locator(
    "section.project-service-attention-panel",
  );
  await expect(
    attentionPanel.getByText("Build required").first(),
  ).toBeVisible();
  await expect(attentionPanel).toContainText(
    "Services excluded by inactive profiles are not treated as failures",
  );
  await expect(page.locator("section.project-services-panel")).toHaveCount(0);
  await page.getByRole("link", { name: "Services", exact: true }).click();
  const servicesPanel = page.locator("section.project-services-panel");
  await expect(servicesPanel.getByRole("columnheader")).toHaveText([
    "Service",
    "Service runtime",
    "Containers",
    "Health",
    "Image",
    "Build",
    "Pull policy",
    "Profiles",
    "Depends on",
    "Ports",
    "",
  ]);
  await expect(page.getByText("Project Up unavailable.")).toBeVisible();
  await expect(
    page
      .locator(".page-header")
      .getByRole("button", { name: "Up", exact: true }),
  ).toBeDisabled();
  await expect(page.getByText("Build required", { exact: true })).toHaveCount(
    0,
  );
  await expect(
    page.getByRole("cell", {
      name: "Required",
      exact: true,
    }),
  ).toHaveCount(2);
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
    `/#/projects/${encodeURIComponent(buildPolicyProject.uid)}/services`,
  );
  const mixedServiceRow = page.locator("tbody tr").filter({
    hasText: "image-and-build",
  });
  await mixedServiceRow
    .getByRole("button", { name: "Actions for image-and-build Service" })
    .click();
  const mixedUp = await runProjectOperation(
    page,
    page
      .locator("#service-actions-menu")
      .getByRole("menuitem", { name: "Up", exact: true }),
  );
  expect(mixedUp.kind).toBe("compose.up");
  expect(mixedUp.target).toBe("image-and-build");
  expect(mixedUp.output_tail).not.toContain("Building");
  await expect(
    page.getByText("No container", { exact: true }).first(),
  ).toBeVisible();

  await page.goto(
    `/#/projects/${encodeURIComponent(normalProject.uid)}/summary`,
  );
  const pull = await runProjectOperation(
    page,
    page
      .locator(".page-header")
      .getByRole("button", { name: "Pull", exact: true }),
    "Pull",
    "It will not start Containers, build Images, or fall back to a build.",
  );
  expect(pull.kind).toBe("compose.pull");

  const up = await runProjectOperation(
    page,
    page
      .locator(".page-header")
      .getByRole("button", { name: "Up", exact: true }),
    "Up",
    "It always uses --no-build and never builds Images.",
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
    `/#/projects/${encodeURIComponent(normalProject.uid)}/services`,
  );
  const webServiceRow = page.locator("tbody tr").filter({
    has: page
      .locator('td[data-label="Service"]')
      .getByText("web", { exact: true }),
  });
  const openWebServiceAction = async (label) => {
    await webServiceRow
      .getByRole("button", { name: "Actions for web Service" })
      .click();
    return page
      .locator("#service-actions-menu")
      .getByRole("menuitem", { name: label, exact: true });
  };
  const serviceStop = await runProjectOperation(
    page,
    await openWebServiceAction("Stop"),
  );
  expect(serviceStop.kind).toBe("compose.stop");
  expect(serviceStop.target).toBe("web");

  const serviceStart = await runProjectOperation(
    page,
    await openWebServiceAction("Start"),
  );
  expect(serviceStart.kind).toBe("compose.start");
  expect(serviceStart.target).toBe("web");

  const serviceRestart = await runProjectOperation(
    page,
    await openWebServiceAction("Restart"),
    "Restart",
  );
  expect(serviceRestart.kind).toBe("compose.restart");
  expect(serviceRestart.target).toBe("web");

  await expect(
    page.getByText("Excluded by profile", { exact: true }),
  ).toBeVisible();

  await page.goto(
    `/#/projects/${encodeURIComponent(normalProject.uid)}/containers`,
  );

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

test("VM Container actions target one Container and enforce protection", async ({
  page,
}) => {
  const browserFailures = observeBrowserFailures(page);
  await page.goto("/#/home");
  const { host } = liveContext(await currentDashboard(page));
  const containers = await jsonFromPage(
    page,
    `/api/v1/hosts/${encodeURIComponent(host.id)}/containers`,
  );
  const target = containers.find(
    (container) =>
      container.compose_project === "docklattice-acceptance-normal" &&
      container.compose_service === "nolog" &&
      !container.one_off &&
      !container.orphan,
  );
  const protectedContainer = containers.find(
    (container) => container.protected,
  );
  expect(target).toBeTruthy();
  expect(protectedContainer).toBeTruthy();
  const targetName = target.names?.join(", ") || target.id.slice(0, 12);
  const protectedName =
    protectedContainer.names?.join(", ") || protectedContainer.id.slice(0, 12);
  const route = `/#/hosts/${encodeURIComponent(host.id)}/containers`;

  const openActions = async (name) => {
    await page
      .getByRole("button", {
        name: `Actions for ${name}`,
        exact: true,
      })
      .click();
    const menu = page.locator("#container-actions-menu");
    await expect(menu).toBeVisible();
    return menu;
  };

  await page.goto(route);
  let menu = await openActions(targetName);
  const stop = await runProjectOperation(
    page,
    menu.getByRole("menuitem", { name: "Stop", exact: true }),
  );
  expect(stop.kind).toBe("container.stop");
  expect(stop.target).toBe(target.id);

  await page.goto(route);
  menu = await openActions(targetName);
  await expect(
    menu.getByRole("menuitem", { name: "Start", exact: true }),
  ).toBeEnabled();
  await expect(
    menu.getByRole("menuitem", { name: "Remove", exact: true }),
  ).toBeEnabled();
  await menu.getByRole("menuitem", { name: "Remove", exact: true }).click();
  const removeConfirmation = page.locator("#confirm-dialog");
  await expect(removeConfirmation).toContainText(
    "does not remove attached Volumes",
  );
  await removeConfirmation.getByRole("button", { name: "Cancel" }).click();

  menu = await openActions(targetName);
  const start = await runProjectOperation(
    page,
    menu.getByRole("menuitem", { name: "Start", exact: true }),
  );
  expect(start.kind).toBe("container.start");
  expect(start.target).toBe(target.id);

  await page.goto(route);
  menu = await openActions(targetName);
  const restart = await runProjectOperation(
    page,
    menu.getByRole("menuitem", { name: "Restart", exact: true }),
    "Restart",
    "This interrupts only the selected Container.",
  );
  expect(restart.kind).toBe("container.restart");
  expect(restart.target).toBe(target.id);

  await page.goto(route);
  menu = await openActions(protectedName);
  await expect(menu).toContainText("protected");
  for (const label of ["Stop", "Restart", "Remove"]) {
    await expect(
      menu.getByRole("menuitem", { name: label, exact: true }),
    ).toBeDisabled();
  }
  await page.keyboard.press("Escape");
  await expect(menu).toBeHidden();

  expect(browserFailures).toEqual([]);
});

test("VM runtime distinguishes one-off and orphan Containers and exposes live Inspectors", async ({
  page,
}) => {
  const browserFailures = observeBrowserFailures(page);
  const projectDirectory = "/opt/docklattice-acceptance/fixtures/stacks/normal";
  const composeRunner = [
    "docker run --rm",
    "--user 65532:65532",
    `--group-add ${vmDockerSocketGID}`,
    "--volume /var/run/docker.sock:/var/run/docker.sock",
    `--volume ${projectDirectory}:${projectDirectory}`,
    `--workdir ${projectDirectory}`,
    "--entrypoint docker",
    "docklattice-ui-vm:agent",
  ].join(" ");

  await vmExec(
    [
      `${composeRunner} compose --file compose-orphan.yaml up --detach orphan`,
      "docker inspect docklattice-acceptance-one-off >/dev/null 2>&1 || " +
        `${composeRunner} compose --file compose.yaml run --detach ` +
        "--no-deps --name docklattice-acceptance-one-off worker",
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
    `/#/projects/${encodeURIComponent(normalProject.uid)}/services`,
  );
  await expect(
    page.getByText("Excluded by profile", { exact: true }),
  ).toBeVisible();

  await page.goto(
    `/#/projects/${encodeURIComponent(normalProject.uid)}/containers`,
  );
  await expect(page.getByText("One-off", { exact: true })).toBeVisible();
  await expect(page.getByText("Orphan", { exact: true })).toBeVisible();

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

  const resizeHandle = page.locator("#inspector-resize-handle");
  await expect(resizeHandle).toBeVisible();
  const initialInspectorWidth = (await inspector.boundingBox()).width;
  const handleBox = await resizeHandle.boundingBox();
  expect(handleBox).not.toBeNull();
  await page.mouse.move(handleBox.x + handleBox.width / 2, handleBox.y + 100);
  await page.mouse.down();
  await page.mouse.move(handleBox.x - 72, handleBox.y + 100);
  await page.mouse.up();
  const draggedInspectorWidth = (await inspector.boundingBox()).width;
  expect(draggedInspectorWidth).toBeGreaterThan(initialInspectorWidth + 50);
  expect(
    await page.evaluate(() =>
      localStorage.getItem("docklattice.inspector-width.v1"),
    ),
  ).toBe(String(Math.round(draggedInspectorWidth)));

  const networkName = "docklattice-acceptance-normal_acceptance-net";
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
  await expect(inspector).toContainText("docklattice-acceptance-normal-web-1");
  expect(
    Math.abs((await inspector.boundingBox()).width - draggedInspectorWidth),
  ).toBeLessThanOrEqual(1);

  await resizeHandle.focus();
  await page.keyboard.press("ArrowRight");
  const keyboardInspectorWidth = (await inspector.boundingBox()).width;
  expect(draggedInspectorWidth - keyboardInspectorWidth).toBe(16);

  const volumeName = "docklattice-acceptance-normal_acceptance-data";
  await page.goto(
    `/#/hosts/${encodeURIComponent(host.id)}/volumes` +
      `?inspect=${encodeURIComponent(volumeName)}`,
  );
  await expect(inspector).toContainText("Containers using this Volume");
  await expect(inspector).toContainText("docklattice-acceptance-normal-web-1");
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
  await expect(inspector).toContainText("Containers using this Image");
  await expect(inspector).toContainText("docklattice-acceptance-normal-web-1");
  expect(
    Math.abs((await inspector.boundingBox()).width - keyboardInspectorWidth),
  ).toBeLessThanOrEqual(1);
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth - window.innerWidth,
    ),
  ).toBeLessThanOrEqual(1);

  await page.goto(`/#/hosts/${encodeURIComponent(host.id)}/containers`);
  const agentRow = page.locator("tbody tr").filter({
    hasText: "docklattice-agent",
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
  const refreshInterval = page.locator("#refresh-interval");
  await expect(refreshInterval.locator("option")).toHaveText([
    "Auto off",
    "Every 15s",
    "Every 30s",
    "Every 1m",
    "Every 5m",
  ]);
  await refreshInterval.selectOption("300000");
  await expect(refreshInterval).toHaveAttribute(
    "title",
    "Log output already updates as a live stream",
  );
  await refreshInterval.selectOption("0");
  await expect(page.locator(".logs-field > span")).toHaveText([
    "Service",
    "Container",
    "Tail",
    "Since",
    "Until",
  ]);
  const serviceSelect = page.locator("#logs-services");
  const containerSelect = page.locator("#logs-container");
  await expect(serviceSelect.locator("option")).toContainText([
    "All Services",
    "nolog",
    "profiled",
    "web",
    "worker",
  ]);
  await serviceSelect.selectOption("web");
  await expect(containerSelect.locator("option")).toHaveCount(2);
  await expect(containerSelect.locator("option").first()).toHaveText(
    "All Containers in web",
  );
  const webOption = containerSelect.locator("option").filter({
    hasText: "docklattice-acceptance-normal-web-1",
  });
  const webContainerID = await webOption.getAttribute("value");
  expect(webContainerID).toBeTruthy();
  await expect(page.locator("#logs-agent")).toHaveAttribute("type", "hidden");
  const logFilterColumnCount = await page
    .locator(".logs-filter-grid")
    .evaluate((element) => {
      return getComputedStyle(element).gridTemplateColumns.split(" ").length;
    });
  expect(logFilterColumnCount).toBe(5);
  const logControlsBox = await page.locator(".logs-controls").boundingBox();
  expect(logControlsBox).not.toBeNull();
  expect(logControlsBox.height).toBeLessThan(150);
  await page.screenshot({
    path: `${evidenceDirectory}/project-logs-controls.png`,
    fullPage: false,
  });
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

  await serviceSelect.selectOption("nolog");
  await expect(containerSelect.locator("option").first()).toHaveText(
    "All Containers in nolog",
  );
  const noLogOption = containerSelect.locator("option").filter({
    hasText: "docklattice-acceptance-normal-nolog-1",
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
  const firstSourceGroup = page.locator(".source-group").first();
  const firstSourceHeading = firstSourceGroup.getByRole("heading");
  const firstSourceItem = firstSourceGroup.locator(".source-link").first();
  await expect(firstSourceHeading).toBeVisible();
  await expect(firstSourceItem).toBeVisible();
  expect(
    await firstSourceHeading.evaluate((element) => {
      return getComputedStyle(element).borderLeftWidth;
    }),
  ).toBe("3px");
  expect(
    await firstSourceItem.evaluate((element) => {
      return getComputedStyle(element).paddingLeft;
    }),
  ).toBe("24px");
  await page.screenshot({
    path: `${evidenceDirectory}/project-files-hierarchy.png`,
    fullPage: true,
  });
  await page
    .getByRole("button", { name: "docker compose config (masked)" })
    .click();
  await expect(page.locator("#editor-title")).toHaveText(
    "docker compose config — masked",
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
    "docker compose config output may contain resolved sensitive values.",
  );
  await revealDialog.getByRole("button", { name: "Reveal" }).click();
  await expect(page.locator("#editor-title")).toHaveText(
    "docker compose config — revealed",
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
  let originalContent = await editor.inputValue();
  const cleanOriginalContent = withoutVMFileTestMarkers(originalContent);
  if (cleanOriginalContent !== originalContent) {
    const contaminatedFile = await jsonFromPage(
      page,
      `${projectPath}/files?path=compose.yaml`,
    );
    const cleanupResponse = await page.request.put(`${projectPath}/files`, {
      data: {
        operation_id: `file-fixture-cleanup-${crypto.randomUUID()}`,
        relative_path: "compose.yaml",
        expected_sha256: contaminatedFile.sha256,
        content: cleanOriginalContent,
      },
    });
    expect(cleanupResponse.ok()).toBe(true);
    await waitForOperation(page, await cleanupResponse.json());
    await page
      .getByRole("button", { name: "compose.yaml", exact: true })
      .click();
    await expect
      .poll(async () => editor.inputValue())
      .toBe(cleanOriginalContent);
    originalContent = cleanOriginalContent;
  }

  const backupsBeforeSave = await jsonFromPage(page, `${projectPath}/backups`);
  const backupIDsBeforeSave = new Set(
    backupsBeforeSave.map((backup) => backup.backup_id),
  );
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
    "DockLattice creates a pre-write backup and applies optimistic hash protection.",
  );
  await revealDialog.getByRole("button", { name: "Save" }).click();
  const saveResponse = await saveResponsePromise;
  expect(saveResponse.ok()).toBe(true);
  const saveOperation = await waitForOperation(page, await saveResponse.json());
  expect(saveOperation.kind).toBe("compose.file.write");

  const backupsAfterSave = await jsonFromPage(page, `${projectPath}/backups`);
  const originalBackups = backupsAfterSave.filter((backup) => {
    return (
      !backupIDsBeforeSave.has(backup.backup_id) &&
      backup.trigger === "pre_write" &&
      backup.paths_available &&
      backup.paths.includes("compose.yaml")
    );
  });
  expect(originalBackups).toHaveLength(1);
  const originalBackup = originalBackups[0];

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
  const originalBackupRow = page.locator("tbody tr").filter({
    hasText: originalBackup.backup_id,
  });
  const restoreButton = originalBackupRow.getByRole("button", {
    name: "Restore",
    exact: true,
  });
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

  const restoredFile = await jsonFromPage(
    page,
    `${projectPath}/files?path=compose.yaml`,
  );
  expect(restoredFile.content).toBe(originalContent);
  expect(restoredFile.content).not.toContain("# vm acceptance save ");
  expect(restoredFile.content).not.toContain("# concurrent winner ");

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

  const injectionMarker = "/tmp/docklattice-command-injection-marker";
  await vmExec(
    `docker exec --user 0 docklattice-agent rm -f ${injectionMarker}`,
  );
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
  await vmExec(`docker exec docklattice-agent test ! -e ${injectionMarker}`);

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

test("VM Container stats survives rapid route churn and confirmation focus is contained", async ({
  page,
}) => {
  const browserFailures = observeBrowserFailures(page);
  await page.goto("/#/home");
  const { host, normalProject } = liveContext(await currentDashboard(page));

  await page.goto(`/#/hosts/${encodeURIComponent(host.id)}/metrics`);
  await expect(
    page.getByRole("heading", { name: "Container stats" }),
  ).toBeVisible();
  await expect(page.locator("#metrics-table th")).toHaveText([
    "Name",
    "CPU %",
    "Memory usage / limit",
    "Net I/O (RX / TX)",
    "Block I/O (read / write)",
    "State",
  ]);
  await expect(page.locator("#metrics-status")).toContainText("Observed", {
    timeout: 30_000,
  });
  await expect(page.locator("#metrics-table tbody tr").first()).toContainText(
    "All containers",
  );
  await expect(page.locator("#metrics-table tbody tr").first()).toContainText(
    "No container memory limit",
  );
  await expect(
    page.locator("#metrics-table tbody tr").first(),
  ).not.toContainText("Unbounded");
  await expect(page.locator("#metrics-table")).toContainText(
    "docklattice-acceptance-normal",
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
    page.getByRole("heading", { name: "Host", exact: true }),
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
    await vmExec("docker stop docklattice-agent");
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
    await vmExec("docker start docklattice-agent");
    await waitForHost(
      page,
      (candidate) =>
        candidate.state === "ACTIVE" &&
        candidate.capabilities.connection.enabled,
    );
  }

  try {
    await vmExec(
      `docker exec --user 0 docklattice-agent mv ${composePlugin} ${disabledComposePlugin}`,
    );
    await vmExec("docker restart docklattice-agent");
    const composeFailure = await waitForHost(
      page,
      (candidate) =>
        candidate.state === "ACTIVE" &&
        candidate.capabilities.connection.enabled &&
        candidate.capabilities.docker.enabled &&
        !candidate.capabilities.compose.enabled,
    );
    expect(composeFailure.capabilities.metrics.enabled).toBe(true);
    const composeContainer = await vmExec(
      "docker inspect --format '{{.State.Status}}' docklattice-agent",
    );
    expect(composeContainer.stdout.trim()).toBe("running");
    await page.goto(
      `/#/projects/${encodeURIComponent(normalProject.uid)}/summary`,
    );
    await expect(
      page
        .locator(".page-header")
        .getByRole("button", { name: "Up", exact: true }),
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
    await vmExec("sudo docker restart docklattice-agent");
    const dockerFailure = await waitForHost(
      page,
      (candidate) => candidate.state !== "ACTIVE",
    );
    expect(dockerFailure.capabilities.connection.enabled).toBe(false);
    await expect
      .poll(async () => {
        const result = await vmExec(
          "sudo docker inspect --format '{{.State.Status}}' docklattice-agent",
        );
        return result.stdout.trim();
      })
      .toMatch(/restarting|exited/);
    await page.goto(
      `/#/projects/${encodeURIComponent(normalProject.uid)}/summary`,
    );
    await expect(
      page
        .locator(".page-header")
        .getByRole("button", { name: "Up", exact: true }),
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

  await vmExec("docker restart docklattice-server");
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

  await vmExec("docker restart docklattice-agent");
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
  const volumeName = "docklattice-acceptance-normal_acceptance-data";
  const networkName = "docklattice-acceptance-normal_acceptance-net";
  const orphanNetworkName = "docklattice-acceptance-normal_default";
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
    "Named Volumes and external Networks or Volumes will be retained.",
  );
  expect(down.kind).toBe("compose.down");

  const remaining = await vmExec(
    "docker ps --all " +
      "--filter label=com.docker.compose.project=docklattice-acceptance-normal " +
      "--format '{{.Names}}'",
  );
  const remainingNames = remaining.stdout.trim().split("\n").sort();
  expect(remainingNames).toEqual(
    [
      "docklattice-acceptance-normal-orphan-1",
      "docklattice-acceptance-one-off",
    ].sort(),
  );
  await vmExec(`docker volume inspect ${volumeName}`);
  const retainedNetwork = await vmExec(`docker network inspect ${networkName}`);
  expect(retainedNetwork.stdout).toContain("docklattice-acceptance-one-off");
  const retainedOrphanNetwork = await vmExec(
    `docker network inspect ${orphanNetworkName}`,
  );
  expect(retainedOrphanNetwork.stdout).toContain(
    "docklattice-acceptance-normal-orphan-1",
  );

  const up = await runProjectOperation(
    page,
    page
      .locator(".page-header")
      .getByRole("button", { name: "Up", exact: true }),
    "Up",
    "It always uses --no-build and never builds Images.",
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
