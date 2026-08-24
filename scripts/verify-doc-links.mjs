#!/usr/bin/env node

import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import path from "node:path";

const repositoryRoot = process.cwd();
const documentationRoot = path.join(repositoryRoot, "docs");
const rootDocuments = ["README.md", "CONTRIBUTING.md", "SECURITY.md"];

function markdownFiles(directory) {
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const entryPath = path.join(directory, entry.name);
    if (entry.isDirectory()) {
      return markdownFiles(entryPath);
    }
    return entry.name.endsWith(".md") ? [entryPath] : [];
  });
}

function localTarget(destination) {
  const unwrapped = destination.replace(/^<|>$/g, "");
  if (unwrapped.startsWith("#") || /^[a-z][a-z0-9+.-]*:/i.test(unwrapped)) {
    return null;
  }

  const pathOnly = unwrapped.split(/[?#]/, 1)[0];
  return pathOnly ? decodeURIComponent(pathOnly) : null;
}

const documents = [
  ...rootDocuments.map((document) => path.join(repositoryRoot, document)),
  ...markdownFiles(documentationRoot),
];
const failures = [];
let checkedLinks = 0;

for (const document of documents) {
  const source = readFileSync(document, "utf8");
  const links = source.matchAll(/!?\[[^\]]*\]\(([^)\s]+)(?:\s+[^)]*)?\)/g);

  for (const match of links) {
    const target = localTarget(match[1]);
    if (!target) {
      continue;
    }

    checkedLinks += 1;
    const resolved = path.resolve(path.dirname(document), target);
    if (!existsSync(resolved)) {
      failures.push(
        `${path.relative(repositoryRoot, document)}: ${match[1]} does not exist`,
      );
      continue;
    }

    statSync(resolved);
  }
}

if (failures.length > 0) {
  console.error("Broken local documentation links:");
  for (const failure of failures) {
    console.error(`- ${failure}`);
  }
  process.exit(1);
}

console.log(
  `Documentation links OK: ${checkedLinks} local targets across ${documents.length} files.`,
);
