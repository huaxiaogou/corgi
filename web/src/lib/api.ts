import { clearAllSnapshotCaches } from "./snapshotCache";

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

let initialBuildTag = "";
const buildListeners = new Set<() => void>();
const unauthorizedListeners = new Set<() => void>();

function checkBuildHeader(res: Response) {
  const tag = res.headers.get("X-Corgi-Web");
  if (!tag) return;
  if (!initialBuildTag) {
    initialBuildTag = tag;
  } else if (tag !== initialBuildTag) {
    buildListeners.forEach((fn) => fn());
  }
}

export function onWebBuildChange(fn: () => void): () => void {
  buildListeners.add(fn);
  return () => {
    buildListeners.delete(fn);
  };
}

export function onUnauthorized(fn: () => void): () => void {
  unauthorizedListeners.add(fn);
  return () => {
    unauthorizedListeners.delete(fn);
  };
}

function notifyUnauthorizedIfSessionEndpoint(path: string, status: number) {
  if (status !== 401) return;
  if (path === "/api/login" || path === "/api/pair") return;
  clearAllSnapshotCaches();
  unauthorizedListeners.forEach((fn) => fn());
}

export async function api<T>(path: string, body?: unknown): Promise<T> {
  if (path === "/api/logout") {
    clearAllSnapshotCaches();
  }
  const res = await fetch(path, {
    method: body !== undefined ? "POST" : "GET",
    headers: body !== undefined ? { "Content-Type": "application/json" } : undefined,
    body: body !== undefined ? JSON.stringify(body) : undefined,
    credentials: "same-origin",
  });
  checkBuildHeader(res);
  let data: Record<string, unknown> = {};
  try {
    data = (await res.json()) as Record<string, unknown>;
  } catch {
    // non-json response
  }
  if (!res.ok) {
    notifyUnauthorizedIfSessionEndpoint(path, res.status);
    const msg = typeof data.error === "string" ? data.error : res.statusText;
    throw new ApiError(res.status, msg);
  }
  if (path === "/api/logout") {
    unauthorizedListeners.forEach((fn) => fn());
  }
  return data as T;
}

export async function uploadFile(file: File, machine?: string): Promise<{ path: string; name: string; size: number }> {
  const qs = new URLSearchParams({ name: file.name });
  if (machine) qs.set("machine", machine);
  const res = await fetch(`/api/uploads?${qs.toString()}`, {
    method: "POST",
    body: file,
    credentials: "same-origin",
  });
  checkBuildHeader(res);
  const data = (await res.json()) as { path?: string; name?: string; size?: number; error?: string };
  if (!res.ok || !data.path) {
    notifyUnauthorizedIfSessionEndpoint("/api/uploads", res.status);
    throw new ApiError(res.status, data.error ?? "上传失败");
  }
  return { path: data.path, name: data.name ?? file.name, size: data.size ?? file.size };
}
