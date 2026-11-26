import { openAsBlob } from "node:fs";
import { Agent, FormData, fetch } from "undici";

/** Deploys can take minutes (docker build + healthcheck). */
const LONG = 30 * 60 * 1000;
const dispatcher = new Agent({ headersTimeout: LONG, bodyTimeout: LONG, connectTimeout: 30_000 });

export class ApiError extends Error {
  constructor(
    public status: number,
    public body: unknown,
  ) {
    const msg =
      body && typeof body === "object" && "error" in body ? String((body as { error: unknown }).error) : `HTTP ${status}`;
    super(msg);
  }
}

export interface ClientConfig {
  url: string;
  token: string;
}

export function configFromEnv(env = process.env): ClientConfig {
  const url = (env.DEPLOYMCP_URL ?? "").trim().replace(/\/+$/, "");
  const token = (env.DEPLOYMCP_TOKEN ?? "").trim();
  if (!url || !token) {
    throw new Error(
      "DeployMCP is not configured: set DEPLOYMCP_URL and DEPLOYMCP_TOKEN in the MCP server env " +
        "(the install script on your VPS prints the exact `claude mcp add` command).",
    );
  }
  if (!/^https?:\/\//.test(url)) throw new Error(`DEPLOYMCP_URL must start with https:// (got ${url})`);
  return { url, token };
}

/** Result of an API call: 2xx or a structured 422 (failed deploy) both resolve. */
export interface ApiResult<T = Record<string, unknown>> {
  status: number;
  data: T;
}

export class AgentClient {
  constructor(private cfg: ClientConfig) {}

  private async request<T>(method: string, path: string, body?: FormData | object): Promise<ApiResult<T>> {
    const headers: Record<string, string> = { Authorization: `Bearer ${this.cfg.token}`, Accept: "application/json" };
    let payload: FormData | string | undefined;
    if (body instanceof FormData) {
      payload = body;
    } else if (body !== undefined) {
      headers["Content-Type"] = "application/json";
      payload = JSON.stringify(body);
    }
    let res;
    try {
      res = await fetch(this.cfg.url + path, { method, headers, body: payload, dispatcher });
    } catch (e) {
      const cause = e instanceof Error && e.cause instanceof Error ? `: ${e.cause.message}` : "";
      throw new Error(`cannot reach DeployMCP agent at ${this.cfg.url}${cause}`);
    }
    const text = await res.text();
    let data: unknown;
    try {
      data = text ? JSON.parse(text) : {};
    } catch {
      data = { error: text.slice(0, 500) };
    }
    if (res.ok || res.status === 422) return { status: res.status, data: data as T };
    throw new ApiError(res.status, data);
  }

  async deploy(app: string, bundlePath: string, opts: { domain?: string; port?: number; healthcheckPath?: string }) {
    const fd = new FormData();
    fd.set("bundle", await openAsBlob(bundlePath, { type: "application/gzip" }), "bundle.tar.gz");
    if (opts.domain) fd.set("domain", opts.domain);
    if (opts.port) fd.set("port", String(opts.port));
    if (opts.healthcheckPath) fd.set("healthcheck_path", opts.healthcheckPath);
    return this.request("POST", `/v1/apps/${encodeURIComponent(app)}/deploy`, fd);
  }

  logs(app: string, q: { type: string; lines: number; level?: string }) {
    const p = new URLSearchParams({ type: q.type, lines: String(q.lines), compact: "true" });
    if (q.level) p.set("level", q.level);
    return this.request("GET", `/v1/apps/${encodeURIComponent(app)}/logs?${p}`);
  }

  rollback(app: string, releaseId?: string) {
    return this.request("POST", `/v1/apps/${encodeURIComponent(app)}/rollback`, releaseId ? { release_id: releaseId } : {});
  }

  listApps() {
    return this.request("GET", "/v1/apps");
  }

  envKeys(app: string) {
    return this.request("GET", `/v1/apps/${encodeURIComponent(app)}/env`);
  }

  updateEnv(app: string, set?: Record<string, string>, del?: string[]) {
    return this.request("PUT", `/v1/apps/${encodeURIComponent(app)}/env`, { set: set ?? {}, delete: del ?? [] });
  }
}
