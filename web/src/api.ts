// Minimal same-origin API client. The session rides an HttpOnly cookie; the
// CSRF token from login / auth/me is replayed on every mutating request.

let csrfToken = ''

export function setCsrfToken(t: string) {
  csrfToken = t
}

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function parseError(res: Response): Promise<ApiError> {
  let msg = res.statusText
  try {
    const body = await res.json()
    if (typeof body?.error === 'string') msg = body.error
  } catch {
    /* non-JSON error body; keep statusText */
  }
  return new ApiError(res.status, msg)
}

export async function apiGet<T>(path: string): Promise<T> {
  const res = await fetch(path, { headers: { Accept: 'application/json' } })
  if (!res.ok) throw await parseError(res)
  return res.json()
}

async function apiSend<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: {
      Accept: 'application/json',
      'Content-Type': 'application/json',
      'X-CSRF-Token': csrfToken,
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (!res.ok) throw await parseError(res)
  return res.json()
}

export function apiPost<T>(path: string, body?: unknown): Promise<T> {
  return apiSend('POST', path, body)
}

export function apiPut<T>(path: string, body: unknown): Promise<T> {
  return apiSend('PUT', path, body)
}

export function apiDelete<T>(path: string): Promise<T> {
  return apiSend('DELETE', path)
}
