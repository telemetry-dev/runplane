import { useMemo } from 'react'
import { errorMessage, isApiError } from './client'

export type FormProblems = {
  /** Messages under the form's fields, for the `errors` of a Base UI Form. */
  fields: Record<string, string>
  /** The message of the problems that no field of the form shows. */
  alert: string | undefined
}

type Options = {
  /** The fields that the form shows. Without it, each field of the request body counts. */
  fields?: readonly string[]
  /** Comes off the field names, as "backup_s3." for the S3 fields. */
  prefix?: string
}

/**
 * Splits the error of a failed submit between the form's fields and one
 * message, so each problem shows somewhere. Huma names a field
 * "body.new_password", and an item of a list "body.server_ids[2]", which goes
 * under the field of the list. A problem about a field that the form does not
 * show, as the email of an invite, goes to the message.
 */
export function formProblems(error: unknown, { fields, prefix = '' }: Options = {}): FormProblems {
  const shown: Record<string, string> = {}
  if (!error) return { fields: shown, alert: undefined }
  const rest: string[] = []
  for (const { location = '', message } of isApiError(error) ? (error.errors ?? []) : []) {
    if (!message) continue
    const name = fieldName(location, prefix)
    if (name !== undefined && (!fields || fields.includes(name))) shown[name] ??= message
    else rest.push(message)
  }
  if (rest.length > 0) return { fields: shown, alert: [...new Set(rest)].join(' ') }
  return { fields: shown, alert: Object.keys(shown).length === 0 ? errorMessage(error) : undefined }
}

/**
 * formProblems for a Base UI Form. The Form shows its errors again whenever it
 * gets a new object, so this keeps one object until the error changes; then a
 * field's message goes away when the user edits that field.
 */
export function useFormProblems(error: unknown, { fields, prefix }: Options = {}): FormProblems {
  return useMemo(() => formProblems(error, { fields, prefix }), [error, fields, prefix])
}

function fieldName(location: string, prefix: string): string | undefined {
  if (!location.startsWith('body.')) return undefined
  const name = location.slice('body.'.length).replace(/\[\d+\]$/, '')
  return name.startsWith(prefix) ? name.slice(prefix.length) : name
}
