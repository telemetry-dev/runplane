import createFetchClient from 'openapi-fetch'
import createQueryHooks from 'openapi-react-query'
import { useMemo } from 'react'
import type { components, paths } from './schema'

export type Schemas = components['schemas']
export type Problem = Schemas['ErrorModel']

/**
 * A non-2xx API response. It carries the fields of the problem JSON (RFC 9457)
 * that the API sends, so it matches the error type that $api hooks declare.
 */
export class ApiError extends Error implements Problem {
  readonly status: number
  readonly type: string
  readonly title: string | undefined
  readonly detail: string | undefined
  readonly errors: Problem['errors']

  constructor(status: number, problem: Problem | undefined, fallback: string) {
    super(problem?.detail || problem?.title || fallback)
    this.name = 'ApiError'
    this.status = status
    this.type = problem?.type ?? 'about:blank'
    this.title = problem?.title
    this.detail = problem?.detail
    this.errors = problem?.errors
  }
}

/**
 * Typed fetch client for the Go API. Every request sends the session cookie and
 * the CSRF header. Every non-2xx response rejects with an ApiError, so a resolved
 * call always has `data`.
 */
export const api = createFetchClient<paths>({
  baseUrl: '',
  credentials: 'include',
  headers: { 'X-Runplane-CSRF': '1' },
})

api.use({
  async onResponse({ response }) {
    if (!response.ok) throw await toApiError(response)
  },
})

/**
 * TanStack Query hooks over `api`. Query keys are `[method, path, init]`, for
 * example `['get', '/api/jobs/{id}', { params: { path: { id } } }]`.
 */
export const $api = createQueryHooks(api)

export async function toApiError(response: Response): Promise<ApiError> {
  const problem = parseProblem(await response.text())
  return new ApiError(response.status, problem, response.statusText || `HTTP ${response.status}`)
}

/** A sentence for the user from any thrown value. */
export function errorMessage(error: unknown): string {
  if (error instanceof Error) return error.message
  return String(error)
}

export function isApiError(error: unknown, status?: number): error is ApiError {
  return error instanceof ApiError && (status === undefined || error.status === status)
}

/**
 * fieldErrors for the `errors` prop of a Base UI Form. The Form puts its errors
 * back whenever it gets a new object, so this keeps one object until `error`
 * changes; then a field's message goes away when the user edits that field, and
 * the form can be sent again.
 */
export function useFieldErrors(error: unknown): Record<string, string> {
  return useMemo(() => fieldErrors(error), [error])
}

/**
 * Field messages of a validation problem, keyed by request body field. Huma
 * reports a field as "body.new_password".
 */
export function fieldErrors(error: unknown): Record<string, string> {
  const errors: Record<string, string> = {}
  if (!isApiError(error)) return errors
  for (const item of error.errors ?? []) {
    if (item.location?.startsWith('body.') && item.message) errors[item.location.slice(5)] = item.message
  }
  return errors
}

function parseProblem(text: string): Problem | undefined {
  try {
    const value: unknown = JSON.parse(text)
    return value && typeof value === 'object' ? (value as Problem) : undefined
  } catch {
    return undefined
  }
}
