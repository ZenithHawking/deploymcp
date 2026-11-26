import assert from "node:assert/strict";
import { promises as fs } from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";
import * as tar from "tar";
import { listFiles, packProject } from "../packager.js";

async function makeProject(files: Record<string, string>): Promise<string> {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "dmcp-test-"));
  for (const [rel, body] of Object.entries(files)) {
    const p = path.join(root, rel);
    await fs.mkdir(path.dirname(p), { recursive: true });
    await fs.writeFile(p, body);
  }
  return root;
}

test("listFiles honors defaults, root and nested .gitignore", async () => {
  const root = await makeProject({
    "Dockerfile": "FROM node",
    "src/index.js": "x",
    ".gitignore": "dist/\n*.tmp\n",
    "dist/out.js": "x",
    "a.tmp": "x",
    "node_modules/x/index.js": "x",
    ".git/HEAD": "x",
    ".env": "SECRET=1",
    ".env.production": "SECRET=2",
    ".env.example": "SECRET=",
    "pkg/.gitignore": "generated.txt\n",
    "pkg/generated.txt": "x",
    "pkg/keep.txt": "x",
  });
  const files = await listFiles(root);
  assert.deepEqual(files.sort(), [".env.example", ".gitignore", "Dockerfile", "pkg/.gitignore", "pkg/keep.txt", "src/index.js"]);
});

test("packProject creates a tar.gz with project files", async () => {
  const root = await makeProject({ "Dockerfile": "FROM node", "index.js": "console.log(1)" });
  const b = await packProject(root);
  try {
    const names: string[] = [];
    await tar.t({ file: b.file, onReadEntry: (e) => names.push(e.path) });
    assert.deepEqual(names.sort(), ["Dockerfile", "index.js"]);
    assert.equal(b.fileCount, 2);
  } finally {
    await b.cleanup();
  }
});

test("packProject requires a Dockerfile or compose file", async () => {
  const root = await makeProject({ "index.js": "x" });
  await assert.rejects(packProject(root), /no Dockerfile/);
});

test("packProject enforces size limit", async () => {
  const root = await makeProject({ "Dockerfile": "FROM node", "big.bin": "x".repeat(10) });
  await assert.rejects(packProject(root, 10), /over the/);
});
