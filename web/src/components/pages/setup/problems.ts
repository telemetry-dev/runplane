import { errorMessage, fieldErrors } from '~/api/client'

export type SubmitProblems = {
  /** Messages under the form's fields, for the `errors` of a Base UI Form. */
  errors: Record<string, string>
  /** The message for FormAlert, when none of the form's fields is at fault. */
  alert: string | null
}

/**
 * Splits the error of a failed submit between the form's `fields` and its
 * FormAlert. A problem about a field the form does not show, as the email of
 * an invite, goes to the alert, so the user still sees it.
 */
export function submitProblems(error: unknown, fields: readonly string[]): SubmitProblems {
  if (!error) return { errors: {}, alert: null }
  const all = fieldErrors(error)
  const errors: Record<string, string> = {}
  for (const name of fields) {
    const message = all[name]
    if (message) errors[name] = message
  }
  return { errors, alert: Object.keys(errors).length === 0 ? errorMessage(error) : null }
}
