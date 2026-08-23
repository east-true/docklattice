import { mkdir } from "node:fs/promises";

import { chromium, expect } from "@playwright/test";

import { browserExecutable } from "../../playwright.config.mjs";

const baseURL = process.env.PLAYWRIGHT_TEST_BASE_URL;
const evidenceDirectory =
  process.env.DOCKPILOT_VM_EVIDENCE_DIRECTORY ||
  "test-results/vm-visual-acceptance";

if (!baseURL) {
  throw new Error(
    "PLAYWRIGHT_TEST_BASE_URL is required for live VM visual acceptance.",
  );
}

const viewports = [
  {
    height: 1000,
    name: "desktop-1440",
    route: "container-inspector",
    width: 1440,
  },
  {
    height: 900,
    name: "desktop-1280",
    route: "container-table",
    width: 1280,
  },
  {
    height: 900,
    name: "tablet-1024",
    route: "container-inspector",
    width: 1024,
  },
  {
    height: 900,
    name: "tablet-768",
    route: "host-summary",
    width: 768,
  },
  {
    height: 812,
    name: "mobile-375",
    route: "container-inspector",
    width: 375,
  },
];

function recordBrowserFailures(page) {
  const failures = [];

  page.on("console", (message) => {
    if (message.type() === "error") {
      failures.push(`console: ${message.text()}`);
    }
  });
  page.on("pageerror", (error) => {
    failures.push(`page: ${error.message}`);
  });
  page.on("response", (response) => {
    if (response.status() >= 400) {
      failures.push(`response: ${response.status()} ${response.url()}`);
    }
  });

  return failures;
}

async function loadContext(page) {
  let host;
  await expect
    .poll(
      async () => {
        const dashboard = await page.evaluate(async () => {
          const response = await fetch("/api/v1/dashboard");
          if (!response.ok) {
            return undefined;
          }
          return response.json();
        });
        host = dashboard?.hosts?.find(
          (candidate) => candidate.display_name === "dockpilot-vm-acceptance",
        );
        return Boolean(host?.capabilities?.docker?.enabled);
      },
      {
        intervals: [500, 1_000, 2_000],
        timeout: 90_000,
      },
    )
    .toBe(true);

  let containers;
  await expect
    .poll(
      async () => {
        containers = await page.evaluate(async (agentID) => {
          const response = await fetch(
            `/api/v1/hosts/${encodeURIComponent(agentID)}/containers`,
          );
          if (!response.ok) {
            return undefined;
          }
          return response.json();
        }, host.id);
        return Array.isArray(containers);
      },
      {
        intervals: [500, 1_000, 2_000],
        timeout: 90_000,
      },
    )
    .toBe(true);

  const container = containers.find(
    (candidate) =>
      candidate.compose_service === "web" &&
      candidate.compose_project === "dockpilot-acceptance-normal",
  );

  expect(container, "normal web Container must be present").toBeTruthy();

  return {
    container,
    host,
  };
}

function routeFor(viewport, context) {
  const hostPath = `/#/hosts/${encodeURIComponent(context.host.id)}`;

  if (viewport.route === "host-summary") {
    return `${hostPath}/summary`;
  }
  if (viewport.route === "container-table") {
    return `${hostPath}/containers`;
  }
  return (
    `${hostPath}/containers?inspect=` + encodeURIComponent(context.container.id)
  );
}

async function verifyViewport(browser, viewport) {
  const context = await browser.newContext({
    ignoreHTTPSErrors: true,
    viewport: {
      height: viewport.height,
      width: viewport.width,
    },
  });
  const page = await context.newPage();
  const browserFailures = recordBrowserFailures(page);

  try {
    await page.goto(`${baseURL}/#/home`, {
      waitUntil: "domcontentloaded",
    });
    await expect(page.getByRole("heading", { name: "Home" })).toBeVisible();

    const liveContext = await loadContext(page);
    await page.goto(`${baseURL}${routeFor(viewport, liveContext)}`, {
      waitUntil: "domcontentloaded",
    });

    await expect(
      page.getByRole("heading", { name: "dockpilot-vm-acceptance" }),
    ).toBeVisible();

    const inspector = page.getByRole("complementary", {
      name: /dockpilot-acceptance-normal-web-1/,
    });
    if (viewport.route === "container-inspector") {
      await expect(inspector).toBeVisible();
    }

    if (viewport.width <= 768) {
      await expect(
        page.getByRole("button", { name: "Toggle navigation" }),
      ).toBeVisible();
    }

    const horizontalOverflow = await page.evaluate(
      () => document.documentElement.scrollWidth - window.innerWidth,
    );
    expect(horizontalOverflow).toBeLessThanOrEqual(1);
    expect(browserFailures).toEqual([]);

    await page.screenshot({
      fullPage: viewport.width > 375,
      path: `${evidenceDirectory}/${viewport.name}.png`,
    });

    if (viewport.width === 375) {
      const inspectorColumnCount = await inspector
        .locator(".definition-row")
        .first()
        .evaluate((element) => {
          const columns = getComputedStyle(element).gridTemplateColumns;
          return columns.split(" ").length;
        });
      expect(inspectorColumnCount).toBe(1);

      const inspectorScroll = await inspector.evaluate((element) => ({
        clientHeight: element.clientHeight,
        scrollHeight: element.scrollHeight,
      }));
      expect(inspectorScroll.scrollHeight).toBeGreaterThan(
        inspectorScroll.clientHeight,
      );

      await inspector.evaluate((element) => {
        element.scrollTo({
          top: element.scrollHeight,
        });
      });
      await inspector.screenshot({
        path: `${evidenceDirectory}/${viewport.name}-inspector-end.png`,
      });
    }

    console.log(
      `${viewport.name}: ${viewport.width}x${viewport.height} visual evidence passed`,
    );
  } finally {
    await context.close();
  }
}

await mkdir(evidenceDirectory, {
  recursive: true,
});

const browser = await chromium.launch({
  executablePath: browserExecutable(),
});

try {
  for (const viewport of viewports) {
    await verifyViewport(browser, viewport);
  }
} finally {
  await browser.close();
}
