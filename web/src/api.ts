// Typed client for the engine's REST control plane. Same-origin by
// default (the engine serves this SPA); a different origin and a bearer
// token can be set in localStorage for development against a remote API.

export type Project = {
  id: string;
  name: string;
  main_branch_id: string;
  created_at: string;
};

export type Branch = {
  id: string;
  project_id: string;
  name: string;
  parent_id?: string;
  head_lsn: number;
  base_lsn: number;
  created_at: string;
  created_lsn: number;
  is_deleted: boolean;
  physical_db?: string;
  state?: string;
  checked_out_lsn?: number;
  expires_at?: string | null;
};

export type Entry = {
  id?: string;
  lsn: number;
  timestamp: string;
  project_id: string;
  branch_id: string;
  operation: string;
  collection?: string;
  document_id?: string;
  txn_id?: string;
  actor?: string;
};

// TimeTravelInfo has no json tags server-side; fields arrive as Go names.
export type TimeTravelInfo = {
  BranchID: string;
  BranchName: string;
  EarliestLSN: number;
  LatestLSN: number;
  EarliestTime?: string;
  LatestTime?: string;
  EntryCount: number;
};

export type MergeChange = {
  collection: string;
  document_id: string;
  delete?: boolean;
  document?: Record<string, unknown>;
};

export type MergeConflict = {
  collection: string;
  document_id: string;
  base?: Record<string, unknown>;
  ours?: Record<string, unknown>;
  theirs?: Record<string, unknown>;
};

export type MergePlan = {
  id: string;
  project_id: string;
  source_branch: string;
  target_branch: string;
  source_head: number;
  target_head: number;
  base_lsn: number;
  changes: MergeChange[] | null;
  conflicts: MergeConflict[] | null;
  status: string;
  strategy?: string;
  created_at: string;
  applied_at?: string;
};

export type Meta = {
  version: string;
  read_only: boolean;
  demo?: boolean;
  demo_ttl_minutes?: number;
  native_connections?: boolean;
  actor_scope?: string;
  ddl_capture?: boolean;
};

export type CaptureStatus = {
  branch_id: string;
  state: string;
  actor?: string;
  error?: string;
  updated_at?: string;
  last_captured_at?: string;
  head_lsn?: number;
  last_event_lag_ms?: number;
};

export type DemoSession = { project: string; expires_at: string };

export type Sandbox = { branch: Branch; connection_string?: string };

export type Pin = {
  id?: string;
  project_id: string;
  branch_id: string;
  name: string;
  lsn: number;
  note?: string;
  created_at: string;
};

export type CheckoutInfo = {
  connection_string: string;
  physical_db: string;
  lsn: number;
  collections: number;
  documents: number;
};

export type SandboxInfo = {
  branch: string;
  connection_string: string;
  expires_at: string;
  forked_from: string;
  fork_lsn: number;
  pin?: string;
};

export type UndoResult = {
  from_lsn: number;
  to_lsn: number;
  compensations: number;
  conflicts: number;
  unrecoverable: number;
  dry_run: boolean;
  restored?: number;
  deleted?: number;
};

export const DEMO_SESSION_LOST = "argon:demo-session-lost";
function stored(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}
const base = stored("argon.api") ?? "";
const token = stored("argon.token");
// A renewal supersedes every request started under the previous cookie.
// Query cancellation alone cannot prevent a late response from arriving.
let demoGeneration = 0;

async function req<T>(
  path: string,
  init?: { method?: string; body?: unknown },
): Promise<T> {
  const requestGeneration = demoGeneration;
  const headers: Record<string, string> = {};
  if (token) headers.Authorization = `Bearer ${token}`;
  if (init?.body !== undefined) headers["Content-Type"] = "application/json";
  const res = await fetch(base + path, {
    method: init?.method ?? "GET",
    headers,
    body: init?.body !== undefined ? JSON.stringify(init.body) : undefined,
  });
  const body = await res.json().catch(() => ({}));
  if (!res.ok) {
    if (
      res.status === 401 &&
      requestGeneration === demoGeneration &&
      body.error === "no demo session; POST /api/v1/demo/session first"
    ) {
      window.dispatchEvent(new Event(DEMO_SESSION_LOST));
    }
    throw new Error(body.error ?? `${res.status} ${res.statusText}`);
  }
  if (path === "/api/v1/demo/session") demoGeneration += 1;
  return body as T;
}

const post = <T>(path: string, body?: unknown) =>
  req<T>(path, { method: "POST", body: body ?? {} });
const del = <T>(path: string) => req<T>(path, { method: "DELETE" });

const enc = encodeURIComponent;

export const api = {
  meta: () => req<Meta>("/api/v1/meta"),
  ingesters: () =>
    req<{ ingesters: string[]; count: number; capture?: CaptureStatus[] }>(
      "/api/v1/status/ingesters",
    ),

  demoSession: () => post<DemoSession>("/api/v1/demo/session"),
  demoScenario: () =>
    post<{
      branch: string;
      branches?: string[];
      accepted_branch?: string;
      pin?: string;
      actors: string[];
      hint: string;
    }>("/api/v1/demo/scenario"),

  projects: () => req<{ projects: Project[] | null }>("/api/v1/projects"),
  createProject: (name: string) => post<Project>("/api/v1/projects", { name }),

  branches: (p: string) =>
    req<{ branches: Branch[] | null }>(`/api/v1/projects/${enc(p)}/branches`),
  branch: (p: string, b: string) =>
    req<{ branch: Branch; connection_string?: string }>(
      `/api/v1/projects/${enc(p)}/branches/${enc(b)}`,
    ),
  createBranch: (p: string, name: string, from: string) =>
    post<Branch>(`/api/v1/projects/${enc(p)}/branches`, { name, from }),
  deleteBranch: (p: string, b: string) =>
    del<{ deleted: boolean }>(`/api/v1/projects/${enc(p)}/branches/${enc(b)}`),
  checkout: (p: string, b: string) =>
    post<CheckoutInfo>(
      `/api/v1/projects/${enc(p)}/branches/${enc(b)}/checkout`,
    ),
  release: (p: string, b: string) =>
    post<{ released: boolean }>(
      `/api/v1/projects/${enc(p)}/branches/${enc(b)}/release`,
    ),
  createSnapshot: (p: string, b: string) =>
    post<{ snapshots: number; lsn: number }>(
      `/api/v1/projects/${enc(p)}/branches/${enc(b)}/snapshots`,
    ),

  entries: (p: string, b: string, params: Record<string, string | number>) => {
    const q = new URLSearchParams(
      Object.entries(params).map(([k, v]) => [k, String(v)]),
    );
    return req<{ entries: Entry[] | null; has_more: boolean }>(
      `/api/v1/projects/${enc(p)}/branches/${enc(b)}/entries?${q}`,
    );
  },
  timeTravelInfo: (p: string, b: string) =>
    req<TimeTravelInfo>(
      `/api/v1/projects/${enc(p)}/branches/${enc(b)}/time-travel`,
    ),
  timeTravelSummary: (p: string, b: string, lsn: number) =>
    req<{ lsn: number; collections: Record<string, number> }>(
      `/api/v1/projects/${enc(p)}/branches/${enc(b)}/time-travel/query?lsn=${lsn}`,
    ),
  timeTravelDocs: (
    p: string,
    b: string,
    lsn: number,
    collection: string,
    skip = 0,
    limit = 25,
  ) =>
    req<{
      lsn: number;
      collection: string;
      total: number;
      documents: Record<string, unknown>[];
    }>(
      `/api/v1/projects/${enc(p)}/branches/${enc(b)}/time-travel/query?lsn=${lsn}&collection=${enc(collection)}&skip=${skip}&limit=${limit}`,
    ),

  diff: (p: string, b: string) =>
    req<MergePlan>(`/api/v1/projects/${enc(p)}/branches/${enc(b)}/diff`),
  mergePreview: (p: string, b: string) =>
    post<MergePlan>(
      `/api/v1/projects/${enc(p)}/branches/${enc(b)}/merge-preview`,
    ),
  mergePlans: (p: string) =>
    req<{ plans: MergePlan[] | null }>(`/api/v1/merge-plans?project=${enc(p)}`),
  mergePlan: (id: string) => req<MergePlan>(`/api/v1/merge-plans/${enc(id)}`),
  mergeApply: (id: string, strategy?: string) =>
    post<{ applied: number; conflicts_resolved: number }>(
      `/api/v1/merge-plans/${enc(id)}/apply`,
      strategy ? { strategy } : {},
    ),

  undo: (
    p: string,
    b: string,
    args: {
      from_lsn: number;
      to_lsn?: number;
      actor?: string;
      dry_run: boolean;
    },
  ) =>
    post<UndoResult>(
      `/api/v1/projects/${enc(p)}/branches/${enc(b)}/undo`,
      args,
    ),

  pins: (p: string) =>
    req<{ pins: Pin[] | null }>(`/api/v1/projects/${enc(p)}/pins`),
  createPin: (
    p: string,
    args: { name: string; branch?: string; lsn?: number; note?: string },
  ) => post<Pin>(`/api/v1/projects/${enc(p)}/pins`, args),
  deletePin: (p: string, name: string) =>
    del<{ deleted: boolean }>(`/api/v1/projects/${enc(p)}/pins/${enc(name)}`),
  branchFromPin: (p: string, pin: string, name: string) =>
    post<Branch>(`/api/v1/projects/${enc(p)}/pins/${enc(pin)}/branches`, {
      name,
    }),
  sandboxFromPin: (p: string, pin: string, ttl_minutes?: number) =>
    post<SandboxInfo>(`/api/v1/projects/${enc(p)}/pins/${enc(pin)}/sandboxes`, {
      ...(ttl_minutes ? { ttl_minutes } : {}),
    }),

  sandboxes: (p: string) =>
    req<{ sandboxes: Sandbox[] | null }>(
      `/api/v1/projects/${enc(p)}/sandboxes`,
    ),
  createSandbox: (
    p: string,
    args: { name?: string; from?: string; ttl_minutes?: number },
  ) => post<SandboxInfo>(`/api/v1/projects/${enc(p)}/sandboxes`, args),
  discardSandbox: (p: string, b: string) =>
    del<{ discarded: boolean }>(
      `/api/v1/projects/${enc(p)}/sandboxes/${enc(b)}`,
    ),
  extendSandbox: (p: string, b: string, ttl_minutes: number) =>
    post<{ expires_at: string }>(
      `/api/v1/projects/${enc(p)}/sandboxes/${enc(b)}/extend`,
      {
        ttl_minutes,
      },
    ),
  keepSandbox: (p: string, b: string) =>
    post<{ kept: boolean }>(
      `/api/v1/projects/${enc(p)}/sandboxes/${enc(b)}/keep`,
    ),
};
