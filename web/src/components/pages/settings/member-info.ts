import type { Schemas } from '~/api/client'
import { limits } from '~/api/limits'

export type Members = Schemas['Members']
export type Role = Schemas['User']['role']

const roleNames: Record<Role, string> = { admin: 'Admin', member: 'Member' }

export const roles = limits.User.role.enum.map((value) => ({ value, label: roleNames[value] }))
