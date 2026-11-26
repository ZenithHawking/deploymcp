import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { z } from "zod";
import { AgentClient, ApiError } from "../client.js";
import { packProject } from "../packager.js";

type GetClient = () => AgentClient;
type ToolResult = { content: { type: "text"; text: string }[]; isError?: boolean };

const appName = z
  .string()
  .regex(/^[a-z0-9]([a-z0-9-]{0,48}[a-z0-9])?$/, "lowercase letters, digits and dashes, max 50 chars")
  .describe("App identifier (lowercase letters, digits, dashes), e.g. `my-api`");

function text(s: string, isError = false): ToolResult {
  return { content: [{ type: "text", text: s }], ...(isError ? { isError: true } : {}) };
}

function json(v: unknown): string {
  return JSON.stringify(v, null, 2);
}

/** Wraps a handler so config/network/API errors come back as readable tool errors. */
function safe<A>(fn: (args: A) => Promise<ToolResult>): (args: A) => Promise<ToolResult> {
  return async (args: A) => {
    try {
      return await fn(args);
    } catch (e) {
      if (e instanceof ApiError) return text(`Error (HTTP ${e.status}): ${e.message}`, true);
      return text(`Error: ${e instanceof Error ? e.message : String(e)}`, true);
    }
  };
}

export function registerTools(server: McpServer, client: GetClient): void {
  server.registerTool(
    "deploy_app",
    {
      title: "Deploy app",
      description:
        "Package the local project (honors .gitignore; skips node_modules, .git and .env files), upload it to the VPS, " +
        "build it with Docker (Dockerfile or docker-compose.yml required in project root), healthcheck it, and switch " +
        "traffic with automatic HTTPS. If the new container crashes or fails the healthcheck, the previous release keeps " +
        "serving and the error logs are returned so you can fix the code and deploy again.",
      inputSchema: {
        app_name: appName,
        project_path: z.string().min(1).describe("Absolute path of the project directory on this machine"),
        domain: z.string().optional().describe("Domain to serve the app on, e.g. api.example.com (DNS must point to the VPS)"),
        port: z.number().int().min(1).max(65535).optional().describe("Port the app listens on inside the container (default: Dockerfile EXPOSE, else 3000)"),
        healthcheck_path: z.string().optional().describe("HTTP path that must answer < 500 when healthy (default /)"),
      },
      annotations: { destructiveHint: false, idempotentHint: false, openWorldHint: true },
    },
    safe(async (a) => {
      const bundle = await packProject(a.project_path);
      try {
        const { status, data } = await client().deploy(a.app_name, bundle.file, {
          domain: a.domain,
          port: a.port,
          healthcheckPath: a.healthcheck_path,
        });
        const size = `${(bundle.bytes / 1024).toFixed(0)} KB, ${bundle.fileCount} files`;
        const d = data as Record<string, unknown>;
        if (status === 422 || d.ok === false) {
          return text(
            `DEPLOY FAILED (${d.status}) — release ${d.release_id}. ${d.message}\n` +
              `Uploaded ${size}. Use get_logs with type="build" for the full build log.\n\n${json(d)}`,
            true,
          );
        }
        return text(`DEPLOYED: ${d.url ?? ""} (release ${d.release_id}, ${size})\n\n${json(d)}`);
      } finally {
        await bundle.cleanup();
      }
    }),
  );

  server.registerTool(
    "get_logs",
    {
      title: "Get logs",
      description:
        "Get compacted logs of the running app (type=runtime) or the latest build (type=build). Noise such as ANSI codes, " +
        "progress bars, repeated lines and dependency stack frames is removed. Use level=error to see only errors.",
      inputSchema: {
        app_name: appName,
        type: z.enum(["runtime", "build"]).default("runtime"),
        lines: z.number().int().min(1).max(1000).default(80),
        level: z.enum(["all", "error"]).optional().describe("error = only error lines and their stack traces"),
      },
      annotations: { readOnlyHint: true, openWorldHint: true },
    },
    safe(async (a) => {
      const { data } = await client().logs(a.app_name, { type: a.type, lines: a.lines, level: a.level });
      const d = data as { lines?: string[]; release_id?: string; truncated?: boolean; total_lines?: number };
      const lines = d.lines ?? [];
      const head = `${a.type} logs for ${a.app_name} (release ${d.release_id ?? "?"}, ${lines.length}/${d.total_lines ?? lines.length} lines${d.truncated ? ", tail only" : ""}):`;
      return text(lines.length ? `${head}\n${lines.join("\n")}` : `${head}\n(no log output)`);
    }),
  );

  server.registerTool(
    "rollback_app",
    {
      title: "Roll back app",
      description:
        "Roll the app back to the most recent previously-healthy release, or to a specific release_id (see list_apps). " +
        "The target is healthchecked before traffic moves.",
      inputSchema: {
        app_name: appName,
        release_id: z.string().optional().describe("Release to restore, e.g. rel_20261126_140500"),
      },
      annotations: { destructiveHint: false, idempotentHint: false, openWorldHint: true },
    },
    safe(async (a) => {
      const { status, data } = await client().rollback(a.app_name, a.release_id);
      const d = data as Record<string, unknown>;
      if (status === 422 || d.ok === false) return text(`ROLLBACK FAILED: ${d.message}\n\n${json(d)}`, true);
      return text(`${d.message}\n\n${json(d)}`);
    }),
  );

  server.registerTool(
    "list_apps",
    {
      title: "List apps",
      description:
        "List apps this token can access with container state, domain/URL, current release and the 5 most recent releases.",
      inputSchema: {},
      annotations: { readOnlyHint: true, openWorldHint: true },
    },
    safe(async () => {
      const { data } = await client().listApps();
      const d = data as { apps?: Record<string, unknown>[]; token_scope?: string };
      const apps = d.apps ?? [];
      if (apps.length === 0) return text(`No apps yet (token scope: ${d.token_scope}). Use deploy_app to create one.`);
      const summary = apps
        .map((x) => `- ${x.name}: ${x.state} ${x.url ?? ""} (release ${x.current_release || "none"})`)
        .join("\n");
      return text(`${summary}\n\n${json(d)}`);
    }),
  );

  server.registerTool(
    "manage_env",
    {
      title: "Manage environment variables",
      description:
        "List environment variable NAMES of an app (values are never returned), or set/delete variables. " +
        "Updating recreates the app with the new env; if it fails to start, the change is reverted automatically.",
      inputSchema: {
        app_name: appName,
        action: z.enum(["list_keys", "update"]),
        set: z.record(z.string(), z.string()).optional().describe("Variables to add or change, e.g. {\"DATABASE_URL\": \"...\"}"),
        delete: z.array(z.string()).optional().describe("Variable names to remove"),
      },
      annotations: { destructiveHint: false, openWorldHint: true },
    },
    safe(async (a) => {
      if (a.action === "list_keys") {
        const { data } = await client().envKeys(a.app_name);
        const keys = ((data as { keys?: string[] }).keys ?? []) as string[];
        return text(keys.length ? `Env keys for ${a.app_name} (values hidden):\n${keys.join("\n")}` : `No env vars set for ${a.app_name}.`);
      }
      if (!a.set && !a.delete) return text("Nothing to update: pass `set` and/or `delete`.", true);
      const { status, data } = await client().updateEnv(a.app_name, a.set, a.delete);
      const d = data as { message?: string; keys?: string[]; applied?: unknown };
      const out = `${d.message}\nKeys now: ${(d.keys ?? []).join(", ") || "(none)"}`;
      return status === 422 ? text(`${out}\n\n${json(d.applied)}`, true) : text(out);
    }),
  );
}
