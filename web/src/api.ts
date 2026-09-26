// The GUI talks to forge's REST surface, mounted under /ui/api by the Go
// handler. It is a client of that surface, never a second implementation of
// it: input is validated server-side by the same binding.Normalize every other
// surface uses, so the form cannot drift from what the CLI or MCP accept.

export type Requirement = { kind: string; scope?: string[]; reason?: string };

export type ToolSummary = {
  name: string;
  tool: string;
  op: string;
  summary?: string;
  labels?: string[];
  outputKind: string;
  requires?: string[];
};

export type ToolDetail = {
  name: string;
  tool: string;
  op: string;
  summary?: string;
  description?: string;
  labels?: string[];
  outputKind: string;
  requires?: Requirement[];
  inputSchema: Schema;
};

export type Schema = {
  type?: string | string[];
  title?: string;
  description?: string;
  properties?: Record<string, Schema>;
  required?: string[];
  items?: Schema;
  enum?: unknown[];
  default?: unknown;
  minimum?: number;
  maximum?: number;
  // Anything forge can express that a form cannot; see form.tsx.
  oneOf?: unknown;
  anyOf?: unknown;
  allOf?: unknown;
  prefixItems?: unknown;
};

/** A violation as forge reports it: an RFC 9457 extension member carrying a
 *  JSON Pointer at the exact field that was wrong. */
export type Violation = { pointer: string; message: string };

export type Problem = {
  title?: string;
  detail?: string;
  status?: number;
  violations?: Violation[];
};

const base = "api/v1";

async function json<T>(path: string): Promise<T> {
  const res = await fetch(`${base}/${path}`, { credentials: "same-origin" });
  if (!res.ok) throw await problem(res);
  return (await res.json()) as T;
}

async function problem(res: Response): Promise<Problem> {
  try {
    return (await res.json()) as Problem;
  } catch {
    return { title: `${res.status} ${res.statusText}`, status: res.status };
  }
}

export const listTools = () =>
  json<{ tools: ToolSummary[] }>("tools").then((r) => r.tools ?? []);

export const describeTool = (name: string) =>
  json<ToolDetail>(`tools/${encodeURIComponent(name)}`);

export type Result =
  | { kind: "json"; value: unknown; truncated: boolean }
  | { kind: "text"; value: string; truncated: boolean }
  | { kind: "bytes"; blob: Blob; mediaType: string; truncated: boolean };

/** invoke posts the input and interprets the response.
 *
 *  Truncation arrives in the Forge-Truncated header rather than the body, and
 *  a client that ignores it shows half an answer as though it were whole. */
export async function invoke(name: string, input: unknown): Promise<Result> {
  const res = await fetch(`${base}/tools/${encodeURIComponent(name)}/invoke`, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });

  const truncated = res.headers.get("Forge-Truncated") === "true";
  if (!res.ok) throw await problem(res);

  const type = res.headers.get("Content-Type") ?? "";
  if (type.includes("application/json")) {
    return { kind: "json", value: await res.json(), truncated };
  }
  if (type.startsWith("text/plain")) {
    return { kind: "text", value: await res.text(), truncated };
  }
  // Anything else the Go side has already forced to a download; never render
  // it as a document.
  return { kind: "bytes", blob: await res.blob(), mediaType: type, truncated };
}
