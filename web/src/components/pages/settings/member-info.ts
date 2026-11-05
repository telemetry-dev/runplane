import type { Schemas } from '~/api/client'

export type Members = Schemas['Members']
export type Role = Schemas['User']['role']

export const roles: { value: Role; label: string }[] = [
  { value: 'admin', label: 'Admin' },
  { value: 'member', label: 'Member' },
]
