import { promises as fs } from "node:fs";
import os from "node:os";
import path from "node:path";
import ignore, { type Ignore } from "ignore";
import * as tar from "tar";

/** Default upload cap (compressed), matches the agent. */
export const MAX_BUNDLE_BYTES = 100 * 1024 * 1024;

/** Always excluded, regardless of .gitignore. Secrets go through manage_env. */
const ALWAYS_IGNORE = [
  ".git/",
  "node_modules/",
  ".env",
  ".env.*",
  "!.env.example",
  "!.env.sample",
  "!.env.template",
  ".DS_Store",
  "Thumbs.db",
  "__pycache__/",
  ".venv/",
  "venv/",
  ".pytest_cache/",
  ".mypy_cache/",
  ".turbo/",
  ".next/cache/",
  ".idea/",
  ".vscode/",
  "*.log",
];

export interface Bundle {
  file: string;
  bytes: number;
  fileCount: number;
  cleanup: () => Promise<void>;
}

interface Scope {
  dir: string; // relative to root, posix, "" for root
  ig: Ignore;
}

function toPosix(p: string): string {
  return p.split(path.sep).join("/");
}

async function readGitignore(absDir: string): Promise<Ignore | null> {
  try {
    const txt = await fs.readFile(path.join(absDir, ".gitignore"), "utf8");
    return ignore().add(txt);
  } catch {
    return null;
  }
}

function isIgnored(rel: string, isDir: boolean, scopes: Scope[]): boolean {
  for (const s of scopes) {
    const sub = s.dir === "" ? rel : rel.startsWith(s.dir + "/") ? rel.slice(s.dir.length + 1) : null;
    if (sub === null || sub === "") continue;
    if (s.ig.ignores(isDir ? sub + "/" : sub)) return true;
  }
  return false;
}

/** Lists project files honoring .gitignore files (root and nested) plus defaults. */
export async function listFiles(root: string): Promise<string[]> {
  const base: Scope = { dir: "", ig: ignore().add(ALWAYS_IGNORE) };
  const out: string[] = [];

  async function walk(relDir: string, scopes: Scope[]): Promise<void> {
    const absDir = path.join(root, relDir);
    const gi = await readGitignore(absDir);
    const here = gi ? [...scopes, { dir: relDir, ig: gi }] : scopes;
    const entries = await fs.readdir(absDir, { withFileTypes: true });
    entries.sort((a, b) => a.name.localeCompare(b.name));
    for (const e of entries) {
      const rel = relDir === "" ? e.name : `${relDir}/${e.name}`;
      if (e.isDirectory()) {
        if (isIgnored(rel, true, here)) continue;
        await walk(rel, here);
      } else if (e.isFile() || e.isSymbolicLink()) {
        if (isIgnored(rel, false, here)) continue;
        out.push(rel);
      }
    }
  }

  await walk("", [base]);
  return out;
}

/** Packs projectPath into a temporary .tar.gz. Caller must call cleanup(). */
export async function packProject(projectPath: string, maxBytes = MAX_BUNDLE_BYTES): Promise<Bundle> {
  const root = path.resolve(projectPath);
  const st = await fs.stat(root).catch(() => null);
  if (!st || !st.isDirectory()) {
    throw new Error(`project_path is not a directory: ${root}`);
  }
  const files = (await listFiles(root)).map(toPosix);
  if (files.length === 0) {
    throw new Error(`no files to deploy in ${root} (everything is ignored?)`);
  }
  const hasDeployFile = files.some((f) =>
    ["Dockerfile", "docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"].includes(f),
  );
  if (!hasDeployFile) {
    throw new Error(
      `no Dockerfile or docker-compose.yml in ${root}. Create a Dockerfile that builds the app, ` +
        `listens on 0.0.0.0 and EXPOSEs its port, then deploy again.`,
    );
  }
  const tmpDir = await fs.mkdtemp(path.join(os.tmpdir(), "deploymcp-"));
  const file = path.join(tmpDir, "bundle.tar.gz");
  const cleanup = () => fs.rm(tmpDir, { recursive: true, force: true });
  try {
    await tar.c({ gzip: { level: 6 }, cwd: root, file, portable: true, noDirRecurse: true }, files);
    const { size } = await fs.stat(file);
    if (size > maxBytes) {
      throw new Error(
        `bundle is ${(size / 1048576).toFixed(1)} MB, over the ${(maxBytes / 1048576).toFixed(0)} MB limit. ` +
          `Add large build outputs, datasets or media to .gitignore.`,
      );
    }
    return { file, bytes: size, fileCount: files.length, cleanup };
  } catch (e) {
    await cleanup();
    throw e;
  }
}
