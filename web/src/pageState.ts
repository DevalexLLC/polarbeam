export const PRODUCT_NAME = 'PolarBEAM'

export function documentTitle(label = ''): string {
  return label ? `${label} · ${PRODUCT_NAME}` : PRODUCT_NAME
}

export function isTargetID(value: string): boolean {
  const canonical = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i
  if (/^urn:uuid:/i.test(value)) return canonical.test(value.slice(9))
  if (value.startsWith('{') && value.endsWith('}')) return canonical.test(value.slice(1, -1))
  return /^[0-9a-f]{32}$/i.test(value) || canonical.test(value)
}

export interface PageFailure {
  message: string
  retryable: boolean
}

function errorStatus(error: unknown): number | null {
  if (typeof error !== 'object' || error === null || !('status' in error)) return null
  const status = (error as { status?: unknown }).status
  return typeof status === 'number' ? status : null
}

// Browser/server diagnostic text is deliberately excluded. Operators get a
// stable next step; the original error remains available to console/server
// diagnostics without leaking SQL, routing, or dependency details here.
export function pageFailure(error: unknown, subject: string, notFoundMessage?: string): PageFailure {
  const status = errorStatus(error)
  if (status === 404) {
    return { message: notFoundMessage ?? `The requested ${subject} could not be found.`, retryable: false }
  }
  if (status === 400) return { message: `The request for ${subject} is not valid.`, retryable: false }
  if (status === 401) return { message: 'Your session has expired. Sign in again.', retryable: false }
  if (status === 403) return { message: `You do not have access to the requested ${subject}.`, retryable: false }
  const retryable = status === null || status === 408 || status === 425 || status === 429 || status >= 500
  return {
    message: retryable
      ? `PolarBEAM could not load the ${subject}. Try again.`
      : `PolarBEAM could not load the ${subject}.`,
    retryable,
  }
}
