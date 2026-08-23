import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";

const environment = { ...process.env };
delete environment.NO_COLOR;

const playwrightCLI = fileURLToPath(
  import.meta.resolve("@playwright/test/cli"),
);
const child = spawn(
  process.execPath,
  [playwrightCLI, ...process.argv.slice(2)],
  {
    env: environment,
    stdio: "inherit",
  },
);

for (const signal of ["SIGINT", "SIGTERM"]) {
  process.on(signal, () => child.kill(signal));
}

child.on("error", (error) => {
  console.error(`Unable to start Playwright: ${error.message}`);
  process.exitCode = 1;
});

child.on("exit", (code, signal) => {
  if (signal) {
    process.kill(process.pid, signal);
    return;
  }
  process.exitCode = code ?? 1;
});
