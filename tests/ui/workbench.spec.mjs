import { expect, test } from "@playwright/test";

const capability = (enabled, reason = "") => ({
  enabled,
  ...(reason ? { reason } : {}),
});
const dashboard = {
  hosts: [
    {
      id: "agent-east-1",
      display_name: "edge-docker-01",
      state: "ACTIVE",
      capabilities: {
        connection: capability(true),
        docker: capability(true),
        compose: capability(true),
        discovery: capability(true),
        metrics: capability(true),
        operation_recovery: capability(true),
        fs_read: capability(true),
        fs_write: capability(true),
      },
      project_scan: {
        scanned_at: "2026-08-23T10:00:00Z",
        truncated: false,
      },
    },
    {
      id: "agent-west-2",
      display_name: "warehouse-docker-02",
      state: "ACTIVE",
      capabilities: {
        connection: capability(false, "heartbeat unavailable"),
        docker: capability(false, "Docker Engine probe failed"),
        compose: capability(false, "Docker Engine unavailable"),
        discovery: capability(false, "Last scan incomplete"),
        metrics: capability(false, "Docker Engine unavailable"),
        operation_recovery: capability(true),
        fs_read: capability(false, "Agent filesystem unavailable"),
        fs_write: capability(false, "Agent filesystem unavailable"),
      },
      project_scan: {
        scanned_at: "2026-08-23T09:56:00Z",
        truncated: true,
        stop_reason: "directory budget reached",
      },
    },
  ],
  projects: [
    {
      uid: "project-payments",
      agent_id: "agent-east-1",
      name: "payments",
      working_dir: "/srv/stacks/payments",
      present: true,
      stale: false,
      managed: true,
      read_only: false,
      compose_executable: true,
      drift: "unchanged",
      source_graph_complete: true,
      last_verified_at: "2026-08-23T10:00:00Z",
      last_observed_at: "2026-08-23T10:00:00Z",
      container_ids: ["a".repeat(64), "b".repeat(64)],
      compose_files: ["compose.yaml", "compose.prod.yaml"],
      active_profiles: [],
      pull_services: ["api", "db"],
      project_up_available: false,
      project_up_reason:
        "Dockpilot v1 does not build Images; build-required Services: worker",
      defined_services: [
        {
          name: "api",
          image: "company/api:1.8",
          has_build: true,
          active: true,
          build_required: false,
          pull_available: true,
          up_available: true,
        },
        {
          name: "db",
          image: "postgres:18",
          has_build: false,
          active: true,
          build_required: false,
          pull_available: true,
          up_available: true,
        },
        {
          name: "worker",
          has_build: true,
          active: true,
          build_required: true,
          pull_available: false,
          up_available: false,
          unavailable_reason:
            "Dockpilot v1 does not build Images; this Service has no declared Image",
        },
      ],
    },
    {
      uid: "project-archive",
      agent_id: "agent-east-1",
      name: "archive",
      working_dir: "/srv/stacks/archive",
      present: true,
      stale: true,
      managed: true,
      read_only: false,
      compose_executable: true,
      drift: "unchanged",
      source_graph_complete: true,
      last_verified_at: "2026-08-22T09:00:00Z",
      last_observed_at: "2026-08-22T09:00:00Z",
      container_ids: ["c".repeat(64)],
      compose_files: ["compose.yaml"],
      active_profiles: [],
      pull_services: ["api"],
      project_up_available: true,
      defined_services: [
        {
          name: "api",
          image: "company/archive:1.0",
          active: true,
          build_required: false,
          pull_available: true,
          up_available: true,
        },
      ],
    },
  ],
};

const hostDetail = {
  ...dashboard.hosts[0],
  session_source_ip: "10.0.0.12",
  session_observed_at: "2026-08-23T10:00:02Z",
  docker_api_version: "1.52",
  docker_compose_version: "5.3.1",
  engine_summary: {
    version: "29.1.3",
    containers_total: 12,
    containers_running: 9,
    images: 18,
    cpu_capacity: 8,
    memory_capacity_bytes: 17_179_869_184,
    storage_driver: "overlay2",
    logging_driver: "json-file",
    cgroup_driver: "systemd",
    cgroup_version: "2",
    default_runtime: "runc",
    operating_system: "Ubuntu 24.04.3 LTS",
    os_version: "24.04",
    os_type: "linux",
    architecture: "x86_64",
    kernel_version: "6.8.0",
    docker_root_dir: "/var/lib/docker",
  },
};

const summaryMatrixFrame = {
  agent_id: "agent-east-1",
  observed_at: "2026-08-23T10:00:04Z",
  host: {
    cpu_capacity: 8,
    memory_capacity: 17_179_869_184,
    containers_running: 9,
    containers_total: 12,
    filesystems: [],
    totals: {
      container_count: 9,
      pending_count: 0,
      cpu_percent: 275,
      memory_usage: 4_294_967_296,
      network_rx: 0,
      network_tx: 0,
      block_read: 0,
      block_write: 0,
      restarts: 0,
      memory_limit_unbounded: true,
      memory_percent_known: false,
      health: "none",
      health_unreported: 9,
      uptime_known: true,
    },
  },
  projects: [],
  agent_dropped_frames: 0,
  server_dropped_frames: 0,
  membership_stale: false,
  workload_stale: false,
  context_stale: false,
};

const auditPage = {
  events: [],
  coverage: { established: true, gaps: [], unknown_incarnations: [] },
};
const operations = {
  operations: [
    {
      operation_id: "compose-up-20260823",
      agent_id: "agent-west-2",
      project_uid: "",
      kind: "compose.up",
      status: "running",
      phase: "EXECUTING",
      revision: 4,
      can_cancel: true,
      cancel_mode: "BEST_EFFORT_PARTIAL",
      requested_at: "2026-08-23T09:58:00Z",
      started_at: "2026-08-23T09:58:01Z",
      output_tail: "Pulling declared Images",
    },
  ],
};
const runtime = {
  observed_at: "2026-08-23T10:00:00Z",
  services: [
    {
      name: "api",
      status: "running",
      containers: [
        {
          id: "a".repeat(64),
          names: ["payments-api-1"],
          state: "running",
          image: "company/api:1.8",
          ports: [],
        },
      ],
    },
    {
      name: "db",
      status: "running",
      containers: [
        {
          id: "b".repeat(64),
          names: ["payments-db-1"],
          state: "running",
          image: "postgres:18",
          ports: [],
        },
      ],
    },
    { name: "worker", status: "No container", containers: [] },
  ],
  orphans: [],
};
const containerID = "a".repeat(64);
const container = {
  id: containerID,
  names: ["payments-api-1"],
  image: "company/api:1.8",
  image_id: "c".repeat(64),
  state: "running",
  status: "Up 18 minutes (healthy)",
  health: "healthy",
  compose_project: "payments",
  compose_service: "api",
  one_off: false,
  orphan: false,
  ports: [
    {
      host_ip: "127.0.0.1",
      published_port: 8080,
      target_port: 8080,
      protocol: "tcp",
    },
  ],
  exposed_ports: ["8080/tcp"],
  protected: false,
  exit_code: 0,
  created_at: "2026-08-23T09:41:00Z",
  started_at: "2026-08-23T09:42:00Z",
  oom_killed: false,
  restart_count: 0,
  restart_policy: "unless-stopped",
  stop_signal: "SIGTERM",
  stop_timeout_seconds: 10,
  logging_driver: "json-file",
  command: ["serve", "--port", "8080"],
  entrypoint: ["/usr/local/bin/api"],
  labels: {
    "com.docker.compose.project": "payments",
    "com.docker.compose.service": "api",
  },
  mounts: [
    {
      type: "volume",
      source: "payments-data",
      destination: "/var/lib/api",
      read_write: true,
    },
  ],
  networks: [
    {
      name: "payments_default",
      network_id: "d".repeat(64),
      ipv4: "172.24.0.3/16",
      mac: "02:42:ac:18:00:03",
      aliases: ["api"],
    },
  ],
};

async function mockAPI(page) {
  await page.route("**/api/v1/**", async (route) => {
    const url = new URL(route.request().url());
    let body;
    if (url.pathname === "/api/v1/dashboard") body = dashboard;
    else if (url.pathname === "/api/v1/hosts/agent-east-1") body = hostDetail;
    else if (url.pathname === "/api/v1/operations") body = operations;
    else if (url.pathname === "/api/v1/live/matrix") {
      await route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: `event: matrix\ndata: ${JSON.stringify(summaryMatrixFrame)}\n\n`,
      });
      return;
    } else if (url.pathname.endsWith("/audit")) body = auditPage;
    else if (url.pathname === "/api/v1/projects/project-payments/runtime")
      body = runtime;
    else if (url.pathname === "/api/v1/hosts/agent-east-1/containers")
      body = [container];
    else if (
      url.pathname === `/api/v1/hosts/agent-east-1/containers/${containerID}`
    )
      body = container;
    else {
      await route.fulfill({
        status: 404,
        contentType: "application/json",
        body: JSON.stringify({
          code: "NOT_FOUND",
          message: "fixture route not found",
        }),
      });
      return;
    }
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(body),
    });
  });
}

test.beforeEach(async ({ page }) => {
  await mockAPI(page);
  await page.goto("/#/home");
  await expect(page.getByRole("heading", { name: "Home" })).toBeVisible();
});

test("host-first shell renders deterministic fleet state", async ({
  page,
}, testInfo) => {
  const sidebar = page.getByRole("complementary", {
    name: "Dockpilot navigation",
  });
  await expect(sidebar.getByRole("link", { name: "Search" })).toBeVisible();
  await expect(sidebar.getByRole("link", { name: "Home" })).toBeVisible();
  await expect(
    sidebar.getByRole("link", { name: "edge-docker-01" }),
  ).toBeVisible();
  await expect(
    sidebar.getByRole("link", { name: "warehouse-docker-02" }),
  ).toBeVisible();
  await expect(sidebar.getByRole("link", { name: "payments" })).toHaveCount(0);
  await expect(
    page.getByRole("heading", { name: "Needs attention" }),
  ).toBeVisible();
  await expect(page.getByText("heartbeat unavailable").first()).toBeVisible();
  await expect(
    page.getByRole("columnheader", { name: "Docker Compose" }),
  ).toBeVisible();
  await testInfo.attach(`home-${testInfo.project.name}`, {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });
});

test("Host Summary separates visible Engine details without repeating overview facts", async ({
  page,
}) => {
  await page.goto("/#/hosts/agent-east-1/summary");

  const topLevelPanels = await page
    .locator("#view > section.panel > .panel-header h2")
    .allTextContents();
  expect(topLevelPanels).toEqual(["Host", "Docker Engine", "Compose projects"]);

  const hostPanel = page.locator("section.host-summary-panel");
  const enginePanel = page.locator("section.engine-summary-panel");
  await expect(enginePanel).toBeVisible();
  const technicalPanel = enginePanel.locator(".engine-technical-section");
  await expect(
    technicalPanel.getByRole("heading", {
      name: "Engine technical details",
      exact: true,
    }),
  ).toBeVisible();
  await expect(
    technicalPanel.getByText("Engine API version", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(
    enginePanel.locator("dt").filter({
      hasText: /^Engine version$/,
    }),
  ).toHaveCount(1);
  await expect(
    enginePanel.locator("dt").filter({
      hasText: /^Storage driver$/,
    }),
  ).toHaveCount(1);
  const cpuUsageRow = enginePanel
    .locator("dt")
    .filter({
      hasText: /^CPU used \/ total$/,
    })
    .locator("..");
  await expect(cpuUsageRow.locator("dd")).toHaveText("2.75 / 8 logical CPUs");
  const memoryUsageRow = enginePanel
    .locator("dt")
    .filter({
      hasText: /^Memory used \/ total$/,
    })
    .locator("..");
  await expect(memoryUsageRow.locator("dd")).toHaveText(
    "4.0 GiB / 16.0 GiB (25.0%)",
  );
  const observedUsageRow = enginePanel
    .locator("dt")
    .filter({
      hasText: /^Stats observed$/,
    })
    .locator("..");
  await expect(observedUsageRow.locator("dd")).toContainText("Aug 23, 2026");
  await expect(
    enginePanel.getByText("CPU capacity", {
      exact: true,
    }),
  ).toHaveCount(0);
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
  expect(technicalColumnCount).toBe(page.viewportSize().width > 1050 ? 2 : 1);
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
  const capabilitiesSection = hostPanel.locator(".management-capabilities");
  expect(
    await capabilitiesSection.evaluate((element) => {
      return getComputedStyle(element).borderTopWidth;
    }),
  ).toBe("0px");

  const capabilityColumnCount = await hostPanel
    .locator(".capability-grid")
    .evaluate((element) => {
      return getComputedStyle(element).gridTemplateColumns.split(" ").length;
    });
  const viewportWidth = page.viewportSize().width;
  expect(capabilityColumnCount).toBe(
    viewportWidth > 1050 ? 4 : viewportWidth > 480 ? 2 : 1,
  );
});

test("operation Toast distinguishes started, completed, and failed-to-start states", async ({
  page,
}, testInfo) => {
  let createdOperation;
  let rejectNext = false;
  await page.route("**/api/v1/operations*", async (route) => {
    if (route.request().method() === "POST") {
      if (rejectNext) {
        rejectNext = false;
        await route.fulfill({
          status: 409,
          contentType: "application/json",
          body: JSON.stringify({
            code: "PROJECT_BUSY",
            message: "project is locked by another operation",
          }),
        });
        return;
      }
      const request = route.request().postDataJSON();
      createdOperation = {
        operation_id: request.operation_id,
        agent_id: request.agent_id,
        project_uid: request.project_uid,
        kind: request.kind,
        status: "running",
        phase: "EXECUTING",
        revision: 1,
        partial_effects_possible: false,
        requested_at: "2026-08-23T10:00:03Z",
        started_at: "2026-08-23T10:00:04Z",
      };
      await route.fulfill({
        status: 202,
        contentType: "application/json",
        body: JSON.stringify(createdOperation),
      });
      return;
    }
    if (createdOperation) {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ operations: [createdOperation] }),
      });
      return;
    }
    await route.fallback();
  });
  await page.route(
    "**/api/v1/agents/agent-east-1/operations/*",
    async (route) => {
      createdOperation = {
        ...createdOperation,
        status: "success",
        phase: "FINALIZING",
        revision: 2,
        finished_at: "2026-08-23T10:00:06Z",
      };
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(createdOperation),
      });
    },
  );

  await page.goto("/#/projects/project-payments/summary");
  await page
    .locator(".page-header")
    .getByRole("button", { name: "Pull", exact: true })
    .click();
  const pullConfirmation = page.locator("#confirm-dialog");
  await expect(pullConfirmation).toBeVisible();
  await expect(pullConfirmation).toContainText(
    "It will not start Containers, build Images, or fall back to a build.",
  );
  await pullConfirmation.getByRole("button", { name: "Pull" }).click();

  const toast = page.locator(".toast").first();
  await expect(toast).toHaveAttribute("data-tone", "info");
  await expect(toast.getByText("Pull started", { exact: true })).toBeVisible();
  await expect(toast).toContainText("payments · EXECUTING");
  const operationLink = toast.getByRole("link", { name: "View operation" });
  await expect(operationLink).toHaveAttribute(
    "href",
    /#\/operations\?inspect=/,
  );
  await testInfo.attach(`operation-started-${testInfo.project.name}`, {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });

  await expect(toast).toHaveAttribute("data-tone", "success", {
    timeout: 5_000,
  });
  await expect(
    toast.getByText("Pull completed", { exact: true }),
  ).toBeVisible();
  await testInfo.attach(`operation-completed-${testInfo.project.name}`, {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });
  await operationLink.click();
  await expect(page).toHaveURL(/#\/operations\?inspect=/);
  await expect(
    page.getByRole("complementary", { name: "compose.pull" }),
  ).toBeVisible();

  await page.goto("/#/projects/project-payments/summary");
  rejectNext = true;
  await page
    .locator(".page-header")
    .getByRole("button", { name: "Pull", exact: true })
    .click();
  await page
    .locator("#confirm-dialog")
    .getByRole("button", { name: "Pull" })
    .click();
  const failureToast = page.locator(".toast").first();
  await expect(failureToast).toHaveAttribute("data-tone", "error");
  await expect(failureToast).toHaveAttribute("role", "alert");
  await expect(
    failureToast.getByText("Pull failed to start", { exact: true }),
  ).toBeVisible();
  await expect(failureToast).toContainText(
    "project is locked by another operation",
  );
  await expect(
    failureToast.getByRole("link", { name: "View operation" }),
  ).toHaveCount(0);
  await testInfo.attach(`operation-failed-${testInfo.project.name}`, {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });
});

test("Compose list separates current Container counts from observation time", async ({
  page,
}) => {
  await page.goto("/#/hosts/agent-east-1/compose");

  await expect(
    page.getByRole("heading", {
      name: "edge-docker-01",
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
  await expect(
    page.getByRole("heading", {
      name: "Compose projects",
      exact: true,
      level: 2,
    }),
  ).toBeVisible();

  const headers = await page
    .getByRole("columnheader")
    .evaluateAll((elements) => {
      return elements.map((element) => element.textContent.trim());
    });
  expect(headers).toEqual([
    "Project",
    "Services",
    "Containers",
    "Last observed",
    "Compose config",
    "Needs attention",
  ]);

  const currentRow = page
    .getByRole("link", {
      name: "payments",
      exact: true,
    })
    .locator("xpath=ancestor::tr");
  await expect(currentRow.locator("td").nth(2)).toHaveText("2");
  await expect(currentRow.locator("td").nth(3).locator("time")).toHaveAttribute(
    "datetime",
    "2026-08-23T10:00:00Z",
  );

  const staleRow = page
    .getByRole("link", {
      name: "archive",
      exact: true,
    })
    .locator("xpath=ancestor::tr");
  await expect(staleRow.locator("td").nth(2)).toHaveText("Unavailable");
  await expect(staleRow.locator("td").nth(3).locator("time")).toHaveAttribute(
    "datetime",
    "2026-08-22T09:00:00Z",
  );
  await expect(page.getByText("Last known", { exact: false })).toHaveCount(0);
});

test("list columns resize by pointer and keyboard without page overflow", async ({
  page,
}) => {
  await page.goto("/#/hosts/agent-east-1/compose");

  const tableElement = page.locator("table.resizable-table");
  const resizeHandle = tableElement.getByRole("separator", {
    name: "Resize Containers column",
  });
  const containersHeading = resizeHandle.locator("..");
  const observedHeading = tableElement.getByRole("columnheader", {
    name: "Last observed",
    exact: true,
  });
  await resizeHandle.scrollIntoViewIfNeeded();
  const handleBox = await resizeHandle.boundingBox();
  expect(handleBox).not.toBeNull();

  await page.mouse.move(
    handleBox.x + handleBox.width / 2,
    handleBox.y + handleBox.height / 2,
  );
  await page.mouse.down();
  const dragStartWidths = {
    containers: await containersHeading.evaluate((element) => {
      return element.getBoundingClientRect().width;
    }),
    observed: await observedHeading.evaluate((element) => {
      return element.getBoundingClientRect().width;
    }),
  };
  await page.mouse.move(
    handleBox.x + handleBox.width / 2 + 48,
    handleBox.y + handleBox.height / 2,
  );
  await page.mouse.up();

  const draggedWidth = await containersHeading.evaluate((element) => {
    return element.getBoundingClientRect().width;
  });
  const observedWidthAfterDrag = await observedHeading.evaluate((element) => {
    return element.getBoundingClientRect().width;
  });
  expect(draggedWidth).toBeGreaterThan(dragStartWidths.containers + 5);
  expect(observedWidthAfterDrag).toBeLessThan(dragStartWidths.observed - 5);

  const fittedTable = await tableElement.evaluate((element) => {
    const wrapper = element.closest(".table-wrap");
    return {
      tableWidth: element.getBoundingClientRect().width,
      wrapperClientWidth: wrapper.clientWidth,
      wrapperScrollWidth: wrapper.scrollWidth,
    };
  });
  expect(fittedTable.tableWidth).toBeLessThanOrEqual(
    fittedTable.wrapperClientWidth + 1,
  );
  expect(fittedTable.wrapperScrollWidth).toBeLessThanOrEqual(
    fittedTable.wrapperClientWidth + 1,
  );

  const storedRatios = await page.evaluate(() => {
    const key = Object.keys(localStorage).find((candidate) => {
      return candidate.startsWith("dockpilot.table-widths.v2:Project|Services");
    });
    return key ? JSON.parse(localStorage.getItem(key)) : null;
  });
  expect(storedRatios).toHaveLength(6);
  expect(
    Math.abs(storedRatios.reduce((total, ratio) => total + ratio, 0) - 100),
  ).toBeLessThan(0.1);

  await page.reload();
  const restoredHandle = page
    .locator("table.resizable-table")
    .getByRole("separator", {
      name: "Resize Containers column",
    });
  const restoredHeading = restoredHandle.locator("..");
  const restoredWidth = await restoredHeading.evaluate((element) => {
    return element.getBoundingClientRect().width;
  });
  expect(Math.abs(restoredWidth - draggedWidth)).toBeLessThanOrEqual(1);

  await restoredHandle.focus();
  await page.keyboard.press("ArrowLeft");
  const keyboardWidth = await restoredHeading.evaluate((element) => {
    return element.getBoundingClientRect().width;
  });
  expect(keyboardWidth).toBeLessThan(restoredWidth);
  await expect(restoredHandle).toHaveAttribute(
    "aria-valuenow",
    String(Math.round(keyboardWidth)),
  );

  const restoredTableFit = await page
    .locator("table.resizable-table")
    .evaluate((element) => {
      const wrapper = element.closest(".table-wrap");
      return wrapper.scrollWidth - wrapper.clientWidth;
    });
  expect(restoredTableFit).toBeLessThanOrEqual(1);

  const horizontalPageOverflow = await page.evaluate(() => {
    return document.documentElement.scrollWidth - window.innerWidth;
  });
  expect(horizontalPageOverflow).toBeLessThanOrEqual(1);
});

test("Compose build policy is visible and blocks whole-project Up", async ({
  page,
}, testInfo) => {
  await page.goto("/#/projects/project-payments/summary");
  await expect(page.getByRole("heading", { name: "payments" })).toBeVisible();
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
  await expect(
    page.getByRole("group", {
      name: "Apply or remove Compose project",
    }),
  ).toHaveText(/Pull\s*Up\s*Down/);
  await expect(
    page.getByRole("group", {
      name: "Control existing Containers",
    }),
  ).toHaveText(/Start\s*Stop\s*Restart/);
  const topLevelPanels = await page
    .locator("#view > section.panel > .panel-header h2")
    .allTextContents();
  expect(topLevelPanels).toEqual([
    "Project",
    "Containers",
    "Services needing attention",
  ]);
  const projectPanel = page.locator("section.project-summary-panel");
  const runtimePanel = page.locator("section.project-runtime-panel");
  await expect(
    projectPanel.getByRole("heading", {
      name: "Dockpilot management",
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
  const workerAttention = attentionPanel.locator("tbody tr").filter({
    hasText: "worker",
  });
  await expect(workerAttention.getByText("Build required")).toBeVisible();
  await expect(workerAttention.getByText("No container")).toBeVisible();
  await expect(attentionPanel).toContainText(
    "Services excluded by inactive profiles are not treated as failures",
  );
  await expect(page.locator("section.project-services-panel")).toHaveCount(0);
  const projectMetadataPosition = await projectPanel
    .getByText("Project directory", { exact: true })
    .boundingBox();
  const managementPosition = await projectPanel
    .getByRole("heading", {
      name: "Dockpilot management",
      exact: true,
    })
    .boundingBox();
  expect(projectMetadataPosition.y).toBeLessThan(managementPosition.y);
  await expect(page.getByText("Project Up unavailable.")).toBeVisible();
  await expect(page.getByText("build-required Services: worker")).toBeVisible();
  await expect(
    page
      .locator(".page-header")
      .getByRole("button", { name: "Up", exact: true }),
  ).toBeDisabled();

  await page.getByRole("link", { name: "Services", exact: true }).click();
  await expect(page).toHaveURL(/#\/projects\/project-payments\/services$/);
  const servicesPanel = page.locator("section.project-services-panel");
  await expect(servicesPanel.getByRole("columnheader")).toHaveText([
    "Service",
    "Status",
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
  await expect(
    servicesPanel.getByRole("button", { name: "Pull Images" }),
  ).toHaveCount(0);
  await expect(
    servicesPanel.getByRole("button", { name: "Up Project" }),
  ).toHaveCount(0);
  const apiServiceRow = servicesPanel.locator("tbody tr").filter({
    hasText: "api",
  });
  await expect(apiServiceRow.locator('td[data-label="Containers"]')).toHaveText(
    "1",
  );
  await expect(apiServiceRow.locator('td[data-label="Status"]')).toContainText(
    "running",
  );
  await expect(apiServiceRow.locator('td[data-label="Image"]')).toHaveText(
    "company/api:1.8",
  );
  await expect(
    apiServiceRow.getByRole("button", {
      name: "Actions for api Service",
    }),
  ).toBeVisible();
  await apiServiceRow
    .getByRole("button", { name: "Actions for api Service" })
    .click();
  const serviceActionsMenu = page.locator("#service-actions-menu");
  await expect(serviceActionsMenu).toBeVisible();
  const triggerBounds = await apiServiceRow
    .getByRole("button", { name: "Actions for api Service" })
    .boundingBox();
  const menuBounds = await serviceActionsMenu.boundingBox();
  const menuDoesNotCoverTrigger =
    menuBounds.y >= triggerBounds.y + triggerBounds.height ||
    menuBounds.y + menuBounds.height <= triggerBounds.y;
  expect(menuDoesNotCoverTrigger).toBe(true);
  for (const label of ["Pull", "Up", "Start", "Stop", "Restart"]) {
    await expect(
      serviceActionsMenu.getByRole("menuitem", {
        name: label,
        exact: true,
      }),
    ).toBeEnabled();
  }
  await testInfo.attach(`service-actions-menu-${testInfo.project.name}`, {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });
  await page.keyboard.press("Escape");
  await expect(serviceActionsMenu).toBeHidden();
  const workerServiceRow = servicesPanel.locator("tbody tr").filter({
    hasText: "worker",
  });
  await expect(
    workerServiceRow.getByText("Required", { exact: true }),
  ).toBeVisible();
  await workerServiceRow
    .getByRole("button", { name: "Actions for worker Service" })
    .click();
  await expect(serviceActionsMenu).toContainText(
    "No existing Container for this Service",
  );
  for (const label of ["Pull", "Up", "Start", "Stop", "Restart"]) {
    await expect(
      serviceActionsMenu.getByRole("menuitem", {
        name: label,
        exact: true,
      }),
    ).toBeDisabled();
  }
  await page.keyboard.press("Escape");
  await expect(page.getByText("Project Up unavailable.")).toBeVisible();
  await expect(page.getByText("build-required Services: worker")).toBeVisible();
  await expect(
    page
      .locator(".page-header")
      .getByRole("button", { name: "Up", exact: true }),
  ).toBeDisabled();
  await expect(page.getByRole("cell", { name: "Configured" })).toBeVisible();
  await expect(page.getByRole("cell", { name: "Required" })).toBeVisible();
  await expect(page.getByText("--no-build")).toBeVisible();
  const serviceCellLayout = await servicesPanel
    .locator("tbody td")
    .evaluateAll((cells) => {
      return cells.map((cell) => ({
        overflow: getComputedStyle(cell).overflow,
        whiteSpace: getComputedStyle(cell).whiteSpace,
      }));
    });
  expect(serviceCellLayout.every((cell) => cell.whiteSpace === "nowrap")).toBe(
    true,
  );
  const servicesTableOverflow = await servicesPanel
    .locator("table")
    .evaluate((element) => {
      const wrapper = element.closest(".table-wrap");
      return wrapper.scrollWidth - wrapper.clientWidth;
    });
  expect(servicesTableOverflow).toBeLessThanOrEqual(1);
  await testInfo.attach(`project-build-policy-${testInfo.project.name}`, {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });
});

test("Container rows expose state-aware actions without crowding the table", async ({
  page,
}) => {
  await page.goto("/#/hosts/agent-east-1/containers");
  const containerRow = page.locator("tbody tr").filter({
    hasText: "payments-api-1",
  });
  const actionsButton = containerRow.getByRole("button", {
    name: "Actions for payments-api-1",
  });
  await expect(actionsButton).toBeVisible();
  await expect(page.getByRole("columnheader").last()).toHaveText("");
  const triggerStyle = await actionsButton.evaluate((element) => {
    const style = getComputedStyle(element);
    return {
      backgroundColor: style.backgroundColor,
      borderStyle: style.borderStyle,
    };
  });
  expect(triggerStyle.borderStyle).toBe("none");
  expect(triggerStyle.backgroundColor).toBe("rgba(0, 0, 0, 0)");
  await actionsButton.click();

  const actionsMenu = page.locator("#container-actions-menu");
  await expect(actionsMenu).toBeVisible();
  await expect(actionsMenu).toContainText("running");
  await expect(actionsMenu).toContainText(
    "Direct Docker actions target only this Container",
  );
  await expect(
    actionsMenu.getByRole("menuitem", { name: "Start", exact: true }),
  ).toBeDisabled();
  await expect(
    actionsMenu.getByRole("menuitem", { name: "Stop", exact: true }),
  ).toBeEnabled();
  await expect(
    actionsMenu.getByRole("menuitem", { name: "Restart", exact: true }),
  ).toBeEnabled();
  await expect(
    actionsMenu.getByRole("menuitem", { name: "Remove", exact: true }),
  ).toBeDisabled();

  await actionsMenu
    .getByRole("menuitem", { name: "Restart", exact: true })
    .click();
  const confirmation = page.locator("#confirm-dialog");
  await expect(confirmation).toBeVisible();
  await expect(confirmation).toContainText(
    "This interrupts only the selected Container.",
  );
  await confirmation.getByRole("button", { name: "Cancel" }).click();
});

test("Logs controls stay compact and keep browser-only search near output", async ({
  page,
}, testInfo) => {
  await page.goto("/#/projects/project-payments/logs");
  await expect(
    page.getByRole("heading", {
      name: "payments",
      exact: true,
    }),
  ).toBeVisible();

  await expect(page.locator("#logs-agent")).toHaveAttribute("type", "hidden");
  await expect(page.locator(".logs-field")).toHaveCount(5);

  const viewportWidth = page.viewportSize().width;
  const filterColumnCount = await page
    .locator(".logs-filter-grid")
    .evaluate((element) => {
      return getComputedStyle(element).gridTemplateColumns.split(" ").length;
    });
  expect(filterColumnCount).toBe(
    viewportWidth > 1050 ? 5 : viewportWidth > 480 ? 3 : 1,
  );

  const controlsBox = await page.locator(".logs-controls").boundingBox();
  expect(controlsBox).not.toBeNull();
  const maximumControlsHeight =
    viewportWidth > 1050 ? 150 : viewportWidth > 480 ? 230 : 460;
  expect(controlsBox.height).toBeLessThan(maximumControlsHeight);

  const browserTools = page.locator(".logs-browser-tools");
  await expect(browserTools.getByLabel("Find in loaded logs")).toBeVisible();
  if (viewportWidth > 480) {
    const findBox = await page.locator("#logs-find").boundingBox();
    expect(findBox.width).toBeLessThanOrEqual(321);
  }
  await expect(
    page.getByRole("log", {
      name: "Live project logs",
    }),
  ).toBeVisible();

  const horizontalOverflow = await page.evaluate(() => {
    return document.documentElement.scrollWidth - window.innerWidth;
  });
  expect(horizontalOverflow).toBeLessThanOrEqual(1);

  await testInfo.attach(`logs-compact-controls-${testInfo.project.name}`, {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });
});

test("Files distinguishes source categories from source items", async ({
  page,
}, testInfo) => {
  await page.goto("/#/projects/project-payments/files");
  await expect(
    page.getByRole("heading", {
      name: "payments",
      exact: true,
    }),
  ).toBeVisible();

  const sourceGroups = page.locator(".source-group");
  await expect(sourceGroups).toHaveCount(8);
  const composeFilesGroup = sourceGroups.first();
  const categoryHeading = composeFilesGroup.getByRole("heading", {
    name: "Compose files — merge order",
    exact: true,
  });
  const composeFile = composeFilesGroup.getByRole("button", {
    name: "compose.yaml",
    exact: true,
  });
  await expect(categoryHeading).toBeVisible();
  await expect(composeFile).toBeVisible();

  const categoryStyle = await categoryHeading.evaluate((element) => {
    const style = getComputedStyle(element);
    return {
      backgroundColor: style.backgroundColor,
      borderLeftWidth: style.borderLeftWidth,
      fontSize: style.fontSize,
      textTransform: style.textTransform,
    };
  });
  expect(categoryStyle.backgroundColor).not.toBe("rgba(0, 0, 0, 0)");
  expect(categoryStyle.borderLeftWidth).toBe("3px");
  expect(categoryStyle.fontSize).toBe("10px");
  expect(categoryStyle.textTransform).toBe("uppercase");

  const itemStyle = await composeFile.evaluate((element) => {
    const style = getComputedStyle(element);
    return {
      fontSize: style.fontSize,
      paddingLeft: style.paddingLeft,
    };
  });
  expect(itemStyle.fontSize).toBe("12px");
  expect(itemStyle.paddingLeft).toBe("24px");
  expect(
    await composeFilesGroup.evaluate((element) => {
      return getComputedStyle(element).borderBottomWidth;
    }),
  ).toBe("1px");

  const horizontalOverflow = await page.evaluate(() => {
    return document.documentElement.scrollWidth - window.innerWidth;
  });
  expect(horizontalOverflow).toBeLessThanOrEqual(1);

  await testInfo.attach(`files-source-hierarchy-${testInfo.project.name}`, {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });
});

test("narrow viewport exposes an accessible navigation path without horizontal page clipping", async ({
  page,
}, testInfo) => {
  test.skip(
    !["tablet-768", "mobile-375"].includes(testInfo.project.name),
    "collapsible-navigation viewport only",
  );
  const toggle = page.getByRole("button", { name: "Toggle navigation" });
  await expect(toggle).toBeVisible();
  await expect(toggle).toHaveAttribute("aria-expanded", "false");
  await toggle.click();
  await expect(toggle).toHaveAttribute("aria-expanded", "true");
  await expect(
    page.getByRole("complementary", { name: "Dockpilot navigation" }),
  ).toBeVisible();
  const horizontalOverflow = await page.evaluate(
    () => document.documentElement.scrollWidth - window.innerWidth,
  );
  expect(horizontalOverflow).toBeLessThanOrEqual(1);
});

test("Container Inspector is route-aware, non-modal, and responsive", async ({
  page,
}, testInfo) => {
  await page.goto(`/#/hosts/agent-east-1/containers?inspect=${containerID}`);
  const inspector = page.getByRole("complementary", { name: "payments-api-1" });
  await expect(inspector).toBeVisible();
  await expect(
    page.getByText("Published ports", { exact: true }).last(),
  ).toBeVisible();
  await expect(
    page.getByText("Exposed ports (image config)", { exact: true }),
  ).toBeVisible();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  const geometry = await inspector.evaluate((element) => {
    const rect = element.getBoundingClientRect();
    return {
      left: rect.left,
      right: rect.right,
      width: rect.width,
      viewport: window.innerWidth,
    };
  });
  if (["desktop-1440", "desktop-1280"].includes(testInfo.project.name)) {
    expect(geometry.right).toBe(geometry.viewport);
    expect(geometry.width).toBeGreaterThanOrEqual(500);
  }

  const resizeHandle = page.locator("#inspector-resize-handle");
  if (page.viewportSize().width > 800) {
    await expect(resizeHandle).toBeVisible();
    await expect(resizeHandle).toHaveAttribute("role", "separator");
    await expect(resizeHandle).toHaveAttribute(
      "aria-label",
      "Resize details panel",
    );

    const initialWidth = (await inspector.boundingBox()).width;
    const handleBox = await resizeHandle.boundingBox();
    expect(handleBox).not.toBeNull();
    await page.mouse.move(handleBox.x + handleBox.width / 2, handleBox.y + 80);
    await page.mouse.down();
    await page.mouse.move(handleBox.x - 80, handleBox.y + 80);
    await page.mouse.up();

    const draggedWidth = (await inspector.boundingBox()).width;
    expect(draggedWidth).toBeGreaterThan(initialWidth + 60);
    await expect(resizeHandle).toHaveAttribute(
      "aria-valuenow",
      String(Math.round(draggedWidth)),
    );
    expect(
      await page.evaluate(() =>
        localStorage.getItem("dockpilot.inspector-width.v1"),
      ),
    ).toBe(String(Math.round(draggedWidth)));

    await page.reload();
    await expect(inspector).toBeVisible();
    const restoredWidth = (await inspector.boundingBox()).width;
    expect(Math.abs(restoredWidth - draggedWidth)).toBeLessThanOrEqual(1);

    await resizeHandle.focus();
    await page.keyboard.press("ArrowRight");
    const keyboardWidth = (await inspector.boundingBox()).width;
    expect(keyboardWidth).toBeLessThan(restoredWidth);
    expect(restoredWidth - keyboardWidth).toBe(16);

    if (page.viewportSize().width >= 1280) {
      const workspaceGeometry = await page
        .locator(".workspace")
        .evaluate((element) => ({
          availableContent:
            element.getBoundingClientRect().width -
            Number.parseFloat(getComputedStyle(element).paddingRight),
          paddingRight: Number.parseFloat(
            getComputedStyle(element).paddingRight,
          ),
        }));
      expect(
        Math.abs(workspaceGeometry.paddingRight - keyboardWidth),
      ).toBeLessThanOrEqual(1);
      expect(workspaceGeometry.availableContent).toBeGreaterThanOrEqual(420);
    }
  } else {
    await expect(resizeHandle).toBeHidden();
  }

  const horizontalOverflow = await page.evaluate(
    () => document.documentElement.scrollWidth - window.innerWidth,
  );
  expect(horizontalOverflow).toBeLessThanOrEqual(1);
  if (testInfo.project.name === "mobile-375") {
    expect(Math.abs(geometry.width - geometry.viewport)).toBeLessThanOrEqual(1);

    const definitionColumnCount = await inspector
      .locator(".definition-row")
      .first()
      .evaluate((element) => {
        const columns = getComputedStyle(element).gridTemplateColumns;
        return columns.split(" ").length;
      });
    expect(definitionColumnCount).toBe(1);
  }
  await testInfo.attach(`container-inspector-${testInfo.project.name}`, {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });
});

test("Inspector requests cannot reopen an object after route navigation", async ({
  page,
}) => {
  let releaseDetail;
  await page.route(
    `**/api/v1/hosts/agent-east-1/containers/${containerID}`,
    async (route) => {
      await new Promise((resolve) => {
        releaseDetail = resolve;
      });
      await route
        .fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(container),
        })
        .catch(() => {});
    },
  );

  await page.goto(`/#/hosts/agent-east-1/containers?inspect=${containerID}`);
  await expect(page.getByText("Loading current Docker details…")).toBeVisible();

  await page.evaluate(() => {
    window.location.hash = "#/home";
  });
  await expect(page.getByRole("heading", { name: "Home" })).toBeVisible();
  releaseDetail?.();
  await expect(page.locator("#inspector")).toBeHidden();
});

test("aborted Metrics stream cannot write into a page after route navigation", async ({
  page,
}) => {
  const pageErrors = [];
  let releaseMetrics;
  page.on("pageerror", (error) => {
    pageErrors.push(error.message);
  });
  await page.route("**/api/v1/live/matrix?*", async (route) => {
    await new Promise((resolve) => {
      releaseMetrics = resolve;
    });
    await route
      .fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: "event: matrix\ndata: {}\n\n",
      })
      .catch(() => {});
  });

  await page.goto("/#/hosts/agent-east-1/metrics");
  await expect(page.locator("#metrics-status")).toContainText(
    "Opening container stats",
  );
  await page.evaluate(() => {
    window.location.hash = "#/home";
  });
  await expect(page.getByRole("heading", { name: "Home" })).toBeVisible();

  releaseMetrics?.();
  await page.waitForTimeout(100);
  expect(pageErrors).toEqual([]);
});

test("Container stats explains a missing Container memory limit in Docker terms", async ({
  page,
}) => {
  await page.goto("/#/hosts/agent-east-1/metrics");

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

  const hostWorkloadRow = page
    .locator("#metrics-table tbody tr")
    .filter({ hasText: "All containers" });
  await expect(hostWorkloadRow).toContainText(
    "No container memory limit · 4.0 GiB used",
  );
  await expect(hostWorkloadRow).not.toContainText("Unbounded");
});

test("Operation Center keeps Agent cancelability separate from current reachability", async ({
  page,
}, testInfo) => {
  await page.goto("/#/operations");
  await expect(page.getByRole("heading", { name: "Operations" })).toBeVisible();
  await expect(page.getByText("compose-up-20260823")).toBeVisible();
  await expect(page.getByRole("button", { name: "Cancel" })).toHaveCount(0);
  const operationRow = page
    .getByRole("row")
    .filter({ hasText: "compose-up-20260823" });
  await expect(
    operationRow.locator('[title="heartbeat unavailable"]'),
  ).toBeVisible();
  await operationRow.getByRole("button").click();
  const inspector = page.getByRole("complementary", { name: "compose.up" });
  await expect(inspector).toBeVisible();
  await expect(
    inspector.getByText(
      "Last synchronized Server index. Current Agent detail is unavailable.",
    ),
  ).toBeVisible();
  await expect(inspector.getByText("Pulling declared Images")).toBeVisible();
  await testInfo.attach(`operations-${testInfo.project.name}`, {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });
});
