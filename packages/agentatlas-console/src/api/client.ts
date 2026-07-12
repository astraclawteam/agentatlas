export class SessionRedirect extends Error {
  constructor() {
    super("session redirect in progress");
    this.name = "SessionRedirect";
  }
}

export class ApiError extends Error {
  readonly status: number;
  readonly details: unknown;

  private constructor(status: number, message: string, details: unknown) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.details = details;
  }

  static async from(response: Response): Promise<ApiError> {
    const raw = await response.text();
    let details: unknown = raw;
    let message = raw || `请求失败（${response.status}）`;
    if (raw) {
      try {
        details = JSON.parse(raw) as unknown;
        if (typeof details === "object" && details !== null && "message" in details) {
          const candidate = (details as { message?: unknown }).message;
          if (typeof candidate === "string" && candidate) message = candidate;
        }
      } catch {
        // Preserve a non-JSON response as truthful diagnostic text.
      }
    }
    return new ApiError(response.status, message, details);
  }
}

export interface ApiNavigation {
  returnTo?: string;
  assignLocation?: (url: string) => void;
}

export async function api<T>(
  path: string,
  init: RequestInit = {},
  navigation: ApiNavigation = {},
): Promise<T> {
  const headers = new Headers(init.headers);
  if (!(init.body instanceof FormData) && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }

  const response = await fetch(path, { ...init, credentials: "include", headers });
  if (response.status === 401) {
    const returnTo = navigation.returnTo ?? `${window.location.pathname}${window.location.search}`;
    const target = `/auth/login?return_to=${encodeURIComponent(returnTo)}`;
    (navigation.assignLocation ?? ((url) => window.location.assign(url)))(target);
    throw new SessionRedirect();
  }
  if (!response.ok) throw await ApiError.from(response);
  if (response.status === 204) return undefined as T;
  return response.json() as Promise<T>;
}
