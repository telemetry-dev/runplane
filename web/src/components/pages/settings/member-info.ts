import type { Schemas } from '~/api/client'
import { limits } from '~/api/limits'

export type Members = Schemas['Members']
export type Role = Schemas['User']['role']
type Capabilities = Schemas['Capabilities']

const roleNames: Record<Role, string> = { admin: 'Admin', member: 'Member' }

export const roles = limits.User.role.enum.map((value) => ({ value, label: roleNames[value] }))

/** "Members can re-run jobs, stop runners, and open debug shells, but cannot change settings." */
export function roleSummary(role: Role, capabilities: Capabilities): string {
  const keys = Object.keys(actions) as (keyof Capabilities)[]
  const can = keys.filter((key) => capabilities[key]).map((key) => actions[key])
  const cannot = keys.filter((key) => !capabilities[key]).map((key) => actions[key])
  const who = `${roleNames[role]}s`
  if (cannot.length === 0) return `${who} can ${and.format(can)}.`
  if (can.length === 0) return `${who} cannot ${or.format(cannot)}.`
  return `${who} can ${and.format(can)}, but cannot ${or.format(cannot)}.`
}

const actions: Record<keyof Capabilities, string> = {
  rerun_jobs: 're-run jobs',
  stop_sandboxes: 'stop runners',
  open_shells: 'open debug shells',
  admin: 'change settings',
}

const and = new Intl.ListFormat('en-US', { type: 'conjunction' })
const or = new Intl.ListFormat('en-US', { type: 'disjunction' })
