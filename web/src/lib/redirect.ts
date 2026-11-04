/**
 * The path to open after login. Only paths on this origin pass; "//host",
 * "/\host", and absolute URLs would send the user to another site.
 */
export function safeRedirectPath(value: string | undefined): string {
  if (!value || !value.startsWith('/') || value.startsWith('//') || value.includes('\\')) return '/'
  return value
}
