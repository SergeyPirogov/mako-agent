export class HttpError extends Error {
  constructor(
    readonly status: number,
    readonly body: string,
    message: string,
  ) {
    super(message);
  }
}

export async function requestJson<T>(
  method: string,
  url: string,
  headers: Record<string, string>,
  body?: unknown,
): Promise<T> {
  const res = await fetch(url, {
    method,
    headers: { Accept: "application/json", "Content-Type": "application/json", ...headers },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok) {
    const text = (await res.text()).slice(0, 500);
    throw new HttpError(res.status, text, `${method} ${url} -> ${res.status}: ${text}`);
  }
  const text = await res.text();
  return (text ? JSON.parse(text) : undefined) as T;
}

/** Turns a raw 401/403/404 from a tracker into a message that says what to fix; other errors pass through. */
export function explainAccess(err: unknown, explain: (status: number, body: string) => string | undefined): never {
  if (err instanceof HttpError) {
    const hint = explain(err.status, err.body);
    if (hint) throw new Error(`${hint}\n(${err.message.slice(0, 300)})`);
  }
  throw err;
}
