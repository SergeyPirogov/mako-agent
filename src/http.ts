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

/** For plain-text endpoints (CI job logs). */
export async function requestText(url: string, headers: Record<string, string>): Promise<string> {
  const res = await fetch(url, { headers });
  const text = await res.text();
  if (!res.ok) throw new HttpError(res.status, text.slice(0, 500), `GET ${url} -> ${res.status}`);
  return text;
}

/** A file (an issue's attachment), at most `maxBytes`: a bigger one is refused before it is read in full. */
export async function requestFile(url: string, headers: Record<string, string>, maxBytes: number): Promise<Buffer> {
  const res = await fetch(url, { headers, redirect: "follow" });
  if (!res.ok) throw new HttpError(res.status, "", `GET ${url} -> ${res.status}`);
  const size = Number(res.headers.get("content-length") ?? 0);
  if (size > maxBytes) throw new Error(`${Math.round(size / 1e6)} МБ, більше за ліміт`);
  const data = Buffer.from(await res.arrayBuffer());
  if (data.length > maxBytes) throw new Error(`${Math.round(data.length / 1e6)} МБ, більше за ліміт`);
  // Some hosts answer a missing permission with their sign-in page instead of an error.
  if ((res.headers.get("content-type") ?? "").includes("text/html") && !/\.html?$/i.test(new URL(url).pathname)) {
    throw new Error("замість файлу прийшла сторінка входу: токен не має доступу");
  }
  return data;
}

/** Turns a raw 401/403/404 from a tracker into a message that says what to fix; other errors pass through. */
export function explainAccess(err: unknown, explain: (status: number, body: string) => string | undefined): never {
  if (err instanceof HttpError) {
    const hint = explain(err.status, err.body);
    if (hint) throw new Error(`${hint}\n(${err.message.slice(0, 300)})`);
  }
  throw err;
}
