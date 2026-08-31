import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./tests",
  testMatch: "widget-ui.spec.mjs",
  fullyParallel: false,
  workers: 1,
  reporter: "line",
  use: {
    baseURL: "http://127.0.0.1:4178",
    browserName: "chromium",
    colorScheme: "dark",
    screenshot: "only-on-failure",
    trace: "retain-on-failure",
  },
  webServer: {
    command: "node tests/static-server.mjs",
    url: "http://127.0.0.1:4178",
    reuseExistingServer: false,
    timeout: 10_000,
  },
});
