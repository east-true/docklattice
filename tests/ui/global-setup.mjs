import { existsSync } from "node:fs";

import { chromium } from "@playwright/test";

export default async function globalSetup(config) {
  const configuredExecutable = config.projects.find(
    (project) => project.use.launchOptions?.executablePath,
  )?.use.launchOptions.executablePath;

  const executable = configuredExecutable || chromium.executablePath();
  if (existsSync(executable)) {
    return;
  }

  throw new Error(
    [
      "No usable Chromium browser was found for the DockLattice UI tests.",
      "Run `npm run test:ui:install` once, or set PLAYWRIGHT_EXECUTABLE_PATH to an installed Chrome or Chromium executable.",
      `Expected Playwright Chromium at: ${chromium.executablePath()}`,
    ].join("\n"),
  );
}
