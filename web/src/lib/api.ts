export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

export async function api<T>(path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method: body !== undefined ? "POST" : "GET",
    headers: body !== undefined ? { "Content-Type": "application/json" } : undefined,
    body: body !== undefined ? JSON.stringify(body) : undefined,
    credentials: "same-origin",
  });
  let data: Record<string, unknown> = {};
  try {
    data = (await res.json()) as Record<string, unknown>;
  } catch {
    // non-json response
  }
  if (!res.ok) {
    const msg = typeof data.error === "string" ? data.error : res.statusText;
    throw new ApiError(res.status, msg);
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
  const data = (await res.json()) as { path?: string; name?: string; size?: number; error?: string };
  if (!res.ok || !data.path) {
    throw new ApiError(res.status, data.error ?? "上传失败");
  }
  return { path: data.path, name: data.name ?? file.name, size: data.size ?? file.size };
}
