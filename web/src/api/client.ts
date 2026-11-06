import createFetchClient, { createFinalURL, createQuerySerializer, defaultPathSerializer, type QuerySerializerOptions } from 'openapi-fetch'
import createQueryHooks from 'openapi-react-query'
import type { components, paths } from './schema'

export type Schemas = components['schemas']
type Problem = Schemas['ErrorModel']

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

// The spec gives each array query param as one comma-separated value
// (explode: false), as the API reads it.
const queryStyle: QuerySerializerOptions = { array: { style: 'form', explode: false } }

/**
 * Typed fetch client for the Go API. Every request sends the session cookie and
 * the CSRF header. Every non-2xx response rejects with an ApiError, so a resolved
 * call always has `data`.
 */
export const api = createFetchClient<paths>({
  baseUrl: '',
  credentials: 'include',
  headers: { 'X-Runplane-CSRF': '1' },
  querySerializer: queryStyle,
})

api.use({
  async onResponse({ response }) {
    if (!response.ok) throw await toApiError(response)
  },
})

/**
 * TanStack Query hooks over `api`. The query of a path without init has the
 * key `[method, path]`, which is the start of the key of every query of the
 * path, so invalidateQueries with it reaches each of them.
 */
export const $api = createQueryHooks(api)

type GetPath = { [P in keyof paths]: paths[P] extends { get: { parameters: unknown } } ? P : never }[keyof paths]
type GetParams<P extends GetPath> = Pick<paths[P]['get']['parameters'], 'path' | 'query'>

/** The URL of a GET route that the browser requests itself: a link, the EventSource, or the WebSocket. */
export function apiUrl<P extends GetPath>(
  path: P,
  ...[params]: GetParams<P> extends { path: object } | { query: object } ? [GetParams<P>] : [GetParams<P>?]
): string {
  return createFinalURL(path, {
    baseUrl: '',
    params: params ?? {},
    querySerializer: createQuerySerializer(queryStyle),
    pathSerializer: defaultPathSerializer,
  })
}

/** The retry rule of the queries: network and server failures, not answers like 404 or 403. */
export function retryQuery(failures: number, error: unknown): boolean {
  return failures < 2 && !(isApiError(error) && error.status < 500)
}

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

function parseProblem(text: string): Problem | undefined {
  try {
    const value: unknown = JSON.parse(text)
    return value && typeof value === 'object' ? (value as Problem) : undefined
  } catch {
    return undefined
  }
}
