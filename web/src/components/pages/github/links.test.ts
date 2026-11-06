import { expect, test } from 'vitest'
import type { Schemas } from '~/api/client'
import { repositoryAccess } from './links'

const organization: Schemas['Installation'] = {
  id: 58213907,
  account_login: 'kestrel-hq',
  account_type: 'Organization',
  avatar_url: 'https://avatars.githubusercontent.com/u/148213377?v=4',
  html_url: 'https://github.com/organizations/kestrel-hq/settings/installations/58213907',
  repository_selection: 'all',
  repository_count: 12,
  repositories: [],
  suspended: false,
  suspended_at: null,
}

test.each([
  ['all', 12, 'All repositories · 12'],
  ['selected', 3, '3 selected repositories'],
  ['selected', 1, '1 selected repository'],
] as const)('repositoryAccess with %s and %i repositories is %j', (selection, count, want) => {
  expect(repositoryAccess({ ...organization, repository_selection: selection, repository_count: count })).toBe(want)
})
