import { existsSync } from "node:fs";
import path from "node:path";

import { chromium, defineConfig } from "@playwright/test";

function systemChromeCandidates() {
  if (process.platform === "linux") {
    return [
      "/usr/bin/google-chrome",
      "/usr/bin/google-chrome-stable",
      "/usr/bin/chromium",
      "/usr/bin/chromium-browser",
    ];
  }
  if (process.platform === "darwin") {
    return [
      "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
      "/Applications/Chromium.app/Contents/MacOS/Chromium",
    ];
  }
  if (process.platform === "win32") {
    return [
      process.env.PROGRAMFILES &&
        path.join(
          process.env.PROGRAMFILES,
          "Google",
          "Chrome",
          "Application",
          "chrome.exe",
        ),
      process.env["PROGRAMFILES(X86)"] &&
        path.join(
          process.env["PROGRAMFILES(X86)"],
          "Google",
          "Chrome",
          "Application",
          "chrome.exe",
        ),
      process.env.LOCALAPPDATA &&
        path.join(
          process.env.LOCALAPPDATA,
          "Google",
          "Chrome",
          "Application",
          "chrome.exe",
        ),
      process.env.LOCALAPPDATA &&
        path.join(
          process.env.LOCALAPPDATA,
          "Chromium",
          "Application",
          "chrome.exe",
        ),
    ].filter(Boolean);
  }
  return [];
}

export function browserExecutable() {
  if (process.env.PLAYWRIGHT_EXECUTABLE_PATH) {
    return process.env.PLAYWRIGHT_EXECUTABLE_PATH;
  }
  if (existsSync(chromium.executablePath())) {
    return undefined;
  }
  return systemChromeCandidates().find((candidate) => existsSync(candidate));
}

const externalBaseURL = process.env.PLAYWRIGHT_TEST_BASE_URL;
const baseURL = externalBaseURL || "http://127.0.0.1:4173";
const executablePath = browserExecutable();

export default defineConfig({
  testDir: "./tests/ui",
  globalSetup: "./tests/ui/global-setup.mjs",
  fullyParallel: true,
  forbidOnly: Boolean(process.env.CI),
  retries: process.env.CI ? 1 : 0,
  workers: process.env.CI ? 1 : undefined,
  reporter: [["list"], ["html", { open: "never" }]],
  use: {
    baseURL,
    colorScheme: "light",
    locale: "en-US",
    timezoneId: "UTC",
    screenshot: "only-on-failure",
    trace: "retain-on-failure",
    launchOptions: executablePath ? { executablePath } : {},
  },
  projects: [
    { name: "desktop-1440", use: { viewport: { width: 1440, height: 1000 } } },
    { name: "desktop-1280", use: { viewport: { width: 1280, height: 900 } } },
    { name: "tablet-1024", use: { viewport: { width: 1024, height: 900 } } },
    { name: "tablet-768", use: { viewport: { width: 768, height: 900 } } },
    { name: "mobile-375", use: { viewport: { width: 375, height: 812 } } },
  ],
  webServer: externalBaseURL
    ? undefined
    : {
        command: "node tests/ui/server.mjs",
        url: baseURL,
        reuseExistingServer: false,
        stdout: "ignore",
        stderr: "pipe",
      },
});
