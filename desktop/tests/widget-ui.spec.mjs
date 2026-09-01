import { expect, test } from "@playwright/test";

const dashboard = {
  hosts: [
    {
      id: "agent-a",
      display_name: "workstation",
      capabilities: {
        connection: { enabled: true },
        compose: { enabled: true },
      },
    },
  ],
  projects: [
    {
      uid: "project-a",
      agent_id: "agent-a",
      name: "checkout",
      working_dir: "/srv/checkout",
      compose_executable: true,
      present: true,
      read_only: false,
      restore_recovery_required: false,
      pull_services: ["api"],
      project_up_available: true,
      defined_services: [
        {
          name: "api",
          image: "company/api:1.8",
          pull_available: true,
          up_available: true,
        },
      ],
    },
  ],
};

const runtime = {
  project_uid: "project-a",
  observed_at: "2026-08-31T00:00:00Z",
  services: [
    {
      name: "api",
      status: "running",
      containers: [{ id: "container-a", state: "running", one_off: false }],
    },
  ],
  orphans: [],
};

async function installBridge(page, customDashboard = dashboard) {
  await page.addInitScript(
    ({ dashboardValue, runtimeValue }) => {
      window.__operationCalls = [];
      window.__holdOperation = false;
      window.__failRuntime = false;
      window.__trayStatusCalls = [];
      window.__windowCalls = [];
      window.__TAURI__ = {
        core: {
          invoke: async (command, payload) => {
            if (command === "dashboard") return dashboardValue;
            if (command === "project_runtime") {
              if (window.__failRuntime) {
                throw new Error("Agent runtime unavailable");
              }
              return runtimeValue;
            }
            if (command === "set_tray_status") {
              window.__trayStatusCalls.push(payload.status);
              return null;
            }
            if (command === "start_operation") {
              window.__operationCalls.push(payload);
              return {
                operation_id: "widget-operation-a",
                agent_id: payload.agentId,
                project_uid: payload.projectUid,
                target: payload.target || "",
                kind: payload.kind,
                status: "running",
              };
            }
            if (command === "operation") {
              if (window.__holdOperation) return new Promise(() => {});
              const started = window.__operationCalls.at(-1);
              return {
                operation_id: "widget-operation-a",
                agent_id: started.agentId,
                project_uid: started.projectUid,
                target: started.target || "",
                kind: started.kind,
                status: "success",
              };
            }
            throw new Error(`Unexpected command: ${command}`);
          },
        },
        window: {
          getCurrentWindow: () => ({
            close: async () => window.__windowCalls.push("close"),
            minimize: async () => window.__windowCalls.push("minimize"),
            startDragging: async () =>
              window.__windowCalls.push("startDragging"),
          }),
        },
      };
    },
    { dashboardValue: customDashboard, runtimeValue: runtime },
  );
}

async function connect(page) {
  await page.goto("/");
  await page.getByLabel("HTTPS Server URL").fill("https://server.test:8080");
  await page.getByRole("button", { name: "Save and connect" }).click();
  await expect(page.getByRole("heading", { name: "checkout" })).toBeVisible();
}

test("compact project controls expose only Pull, Up, Down, Start, and Stop", async ({
  page,
}) => {
  await page.setViewportSize({ width: 360, height: 640 });
  await installBridge(page);
  await connect(page);

  const card = page.locator(".project-card");
  for (const action of ["Pull", "Up", "Down", "Start", "Stop"]) {
    await expect(card.getByRole("button", { name: action })).toBeVisible();
  }
  await expect(card.getByRole("button", { name: "Restart" })).toHaveCount(0);
  await expect
    .poll(() =>
      page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth,
      ),
    )
    .toBe(true);
});

test("Services reveal Docker state and only existing-Container controls", async ({
  page,
}) => {
  await installBridge(page);
  await connect(page);
  await page.getByRole("button", { name: /1 service/ }).click();

  const service = page.locator(".service-row").filter({ hasText: "api" });
  await expect(service).toContainText("running");
  await expect(service.getByRole("button", { name: "Start" })).toBeEnabled();
  await expect(service.getByRole("button", { name: "Stop" })).toBeEnabled();
  await expect(service.getByRole("button", { name: "Pull" })).toHaveCount(0);
  await expect(service.getByRole("button", { name: "Up" })).toHaveCount(0);
});

test("Up asks for confirmation and identifies the Compose project", async ({
  page,
}) => {
  await installBridge(page);
  await connect(page);
  await page.getByRole("button", { name: "Up" }).click();

  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("Up checkout?");
  await expect(dialog).toContainText("always uses --no-build");
  await dialog.getByRole("button", { name: "Up" }).click();
  await expect(page.locator(".toast")).toContainText("Up success", {
    timeout: 4_000,
  });

  const calls = await page.evaluate(() => window.__operationCalls);
  expect(calls).toEqual([
    expect.objectContaining({
      kind: "compose.up",
      projectUid: "project-a",
      target: null,
    }),
  ]);
});

test("keeps a running operation visible beyond the normal toast timeout", async ({
  page,
}) => {
  await page.clock.install();
  await installBridge(page);
  await connect(page);
  await page.evaluate(() => {
    window.__holdOperation = true;
  });

  await page.getByRole("button", { name: "Up" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Up" }).click();
  await expect(page.locator(".toast")).toContainText("Up started");

  await page.clock.fastForward(8_000);
  await expect(page.locator(".toast")).toContainText("Up started");
});

test("offline capability disables every project mutation with its reason", async ({
  page,
}) => {
  const offline = structuredClone(dashboard);
  offline.hosts[0].capabilities.connection = {
    enabled: false,
    reason: "Agent offline",
  };
  await installBridge(page, offline);
  await connect(page);

  const buttons = page.locator(".project-actions .action-button");
  await expect(buttons).toHaveCount(5);
  for (let index = 0; index < 5; index += 1) {
    await expect(buttons.nth(index)).toBeDisabled();
    await expect(buttons.nth(index)).toHaveAttribute("title", "Agent offline");
  }
});

test("runtime refresh failure invalidates Service state and actions", async ({
  page,
}) => {
  await installBridge(page);
  await connect(page);
  await page.getByRole("button", { name: /1 service/ }).click();

  const service = page.locator(".service-row").filter({ hasText: "api" });
  await expect(service).toContainText("running");
  await page.evaluate(() => {
    window.__failRuntime = true;
  });
  await page.getByRole("button", { name: "Refresh data" }).click();

  await expect(service).toContainText("unavailable");
  await expect(service.getByRole("button", { name: "Start" })).toBeDisabled();
  await expect(service.getByRole("button", { name: "Start" })).toHaveAttribute(
    "title",
    "Current Container state is unavailable.",
  );
});

test("changing Servers removes old project data before the new refresh", async ({
  page,
}) => {
  const replacement = structuredClone(dashboard);
  replacement.projects[0].uid = "project-b";
  replacement.projects[0].name = "inventory";

  await page.addInitScript(
    ({ firstDashboard, secondDashboard }) => {
      window.__dashboardCalls = 0;
      window.__TAURI__ = {
        core: {
          invoke: async (command) => {
            if (command === "set_tray_status") return null;
            if (command !== "dashboard") return null;
            window.__dashboardCalls += 1;
            if (window.__dashboardCalls === 1) return firstDashboard;
            return new Promise((resolve) => {
              window.__resolveReplacementDashboard = () =>
                resolve(secondDashboard);
            });
          },
        },
      };
    },
    { firstDashboard: dashboard, secondDashboard: replacement },
  );
  await connect(page);

  await page.getByRole("button", { name: "Connection settings" }).click();
  await page.getByLabel("HTTPS Server URL").fill("https://other.test:8080");
  await page.getByRole("button", { name: "Save and connect" }).click();

  await expect(page.getByRole("heading", { name: "checkout" })).toHaveCount(0);
  await expect(page.locator("#loading-state")).toBeVisible();
  await page.evaluate(() => window.__resolveReplacementDashboard());
  await expect(page.getByRole("heading", { name: "inventory" })).toBeVisible();
});

test("uses a light default theme with a visible connection state", async ({
  page,
}) => {
  await page.setViewportSize({ width: 420, height: 700 });
  await installBridge(page);
  await connect(page);

  await expect(page.locator('meta[name="color-scheme"]')).toHaveAttribute(
    "content",
    "light",
  );
  await expect(page.locator(".connection-state")).toHaveAttribute(
    "data-state",
    "connected",
  );
  await expect(page.locator("#connection-label")).toHaveText(
    "server.test:8080",
  );
  await expect
    .poll(() => page.evaluate(() => window.__trayStatusCalls))
    .toEqual(["disconnected", "connecting", "connected"]);

  const colors = await page.evaluate(() => ({
    background: getComputedStyle(document.body).backgroundColor,
    rootBackground: getComputedStyle(document.documentElement).backgroundColor,
    radius: getComputedStyle(document.querySelector(".widget-shell"))
      .borderRadius,
    shellBackground: getComputedStyle(document.querySelector(".widget-shell"))
      .backgroundColor,
    text: getComputedStyle(document.documentElement).color,
  }));
  expect(colors).toEqual({
    background: "rgba(0, 0, 0, 0)",
    radius: "12px",
    rootBackground: "rgba(0, 0, 0, 0)",
    shellBackground: "rgba(244, 246, 248, 0.96)",
    text: "rgb(24, 33, 43)",
  });
  await expect(
    page.getByRole("button", { name: "Connection settings" }),
  ).toContainText("⚙");
  await expect(page).toHaveTitle("Compose widget");
  await expect(page.locator(".brand-mark")).toHaveCount(0);
  await expect(page.getByText("DockLattice", { exact: true })).toHaveCount(0);

  const screenshotPath = process.env.DOCKLATTICE_WIDGET_SCREENSHOT;
  if (screenshotPath) {
    await page.screenshot({ path: screenshotPath, fullPage: true });
  }
});

test("shows a stable loading shape while the first dashboard is pending", async ({
  page,
}) => {
  await page.addInitScript((dashboardValue) => {
    window.__TAURI__ = {
      core: {
        invoke: (command) => {
          if (command !== "dashboard") {
            throw new Error(`Unexpected command: ${command}`);
          }
          return new Promise((resolve) => {
            window.__resolveDashboard = () => resolve(dashboardValue);
          });
        },
      },
    };
  }, dashboard);

  await page.goto("/");
  await page.getByLabel("HTTPS Server URL").fill("https://server.test:8080");
  await page.getByRole("button", { name: "Save and connect" }).click();

  await expect(page.locator("#loading-state")).toBeVisible();
  await expect(page.locator(".connection-state")).toHaveAttribute(
    "data-state",
    "connecting",
  );
  await page.evaluate(() => window.__resolveDashboard());
  await expect(page.getByRole("heading", { name: "checkout" })).toBeVisible();
  await expect(page.locator("#loading-state")).toBeHidden();
});

test("keeps connection errors inline with a useful recovery state", async ({
  page,
}) => {
  await page.addInitScript(() => {
    window.__TAURI__ = {
      core: {
        invoke: async () => {
          throw new Error("TLS certificate has expired");
        },
      },
    };
  });

  await page.goto("/");
  await page.getByLabel("HTTPS Server URL").fill("https://server.test:8080");
  await page.getByRole("button", { name: "Save and connect" }).click();

  await expect(page.locator(".connection-state")).toHaveAttribute(
    "data-state",
    "error",
  );
  await expect(page.locator("#notice")).toContainText(
    "TLS certificate has expired",
  );
  await expect(page.locator("#empty-state")).toContainText(
    "Project data unavailable",
  );
  await expect(page.locator("#empty-state")).toContainText(
    "Review the Server URL and certificate",
  );
});

test("frameless controls preserve drag, minimize, and close behavior", async ({
  page,
}) => {
  await installBridge(page);
  await connect(page);

  await page
    .locator(".connection-summary")
    .dispatchEvent("pointerdown", { button: 0 });
  await page.getByRole("button", { name: "Minimize window" }).click();
  await page.getByRole("button", { name: "Close window" }).click();

  await expect
    .poll(() => page.evaluate(() => window.__windowCalls))
    .toEqual(["startDragging", "minimize", "close"]);
});
