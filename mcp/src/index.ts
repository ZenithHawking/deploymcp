#!/usr/bin/env node
import { readFileSync } from "node:fs";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { AgentClient, configFromEnv } from "./client.js";
import { registerTools } from "./tools/index.js";

const pkg = JSON.parse(readFileSync(new URL("../package.json", import.meta.url), "utf8")) as { version: string };

if (process.argv.includes("--version")) {
  console.log(pkg.version);
  process.exit(0);
}

const server = new McpServer(
  { name: "deploymcp", version: pkg.version },
  {
    instructions:
      "DeployMCP deploys local projects to the user's VPS as Docker containers with automatic HTTPS. " +
      "Workflow: deploy_app -> if it fails, read the returned logs (or get_logs type=build/runtime), fix the code, deploy again. " +
      "The previous healthy release keeps serving while a new one fails. Secrets: set them with manage_env, never put them in code.",
  },
);

// Config is read lazily so the server still starts (and lists tools) without env vars;
// each tool call then reports a clear configuration error.
let client: AgentClient | undefined;
registerTools(server, () => (client ??= new AgentClient(configFromEnv())));

await server.connect(new StdioServerTransport());
