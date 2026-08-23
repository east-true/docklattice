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
  ],
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
    else if (url.pathname === "/api/v1/operations") body = operations;
    else if (url.pathname.endsWith("/audit")) body = auditPage;
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

test("Compose build policy is visible and blocks whole-project Up", async ({
  page,
}, testInfo) => {
  await page.goto("/#/projects/project-payments/summary");
  await expect(page.getByRole("heading", { name: "payments" })).toBeVisible();
  await expect(page.getByText("Project Up unavailable.")).toBeVisible();
  await expect(page.getByText("build-required Services: worker")).toBeVisible();
  await expect(page.getByRole("button", { name: "Up Project" })).toBeDisabled();
  await expect(
    page.getByRole("cell", { name: "Image + build metadata" }),
  ).toBeVisible();
  await expect(
    page.getByRole("cell", { name: "Build required" }),
  ).toBeVisible();
  await expect(page.getByText("--no-build")).toBeVisible();
  await testInfo.attach(`project-build-policy-${testInfo.project.name}`, {
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
    page.getByText("Image exposed ports", { exact: true }),
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
    "Opening host metrics",
  );
  await page.evaluate(() => {
    window.location.hash = "#/home";
  });
  await expect(page.getByRole("heading", { name: "Home" })).toBeVisible();

  releaseMetrics?.();
  await page.waitForTimeout(100);
  expect(pageErrors).toEqual([]);
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
