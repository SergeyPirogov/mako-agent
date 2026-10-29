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
  if (!res.ok) throw new Error(`${method} ${url} -> ${res.status}: ${(await res.text()).slice(0, 500)}`);
  const text = await res.text();
  return (text ? JSON.parse(text) : undefined) as T;
}
