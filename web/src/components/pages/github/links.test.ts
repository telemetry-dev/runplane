import { expect, test } from 'vitest'
import type { Schemas } from '~/api/client'
import { jobRepositories, repositoryAccess } from './links'

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

// A pull request in a repository that runplane refuses gives jobs that never start.
test.each([
  [false, 'vm', ['kestrel-hq/api']],
  [true, 'vm', ['kestrel-hq/api', 'kestrel-hq/site']],
  [true, 'container', ['kestrel-hq/api']],
] as const)('jobRepositories with allow_public_repos %s in mode %s is %j', (allow, mode, want) => {
  const repositories: Schemas['Repository'][] = [
    { id: 1, full_name: 'kestrel-hq/api', private: true, archived: false },
    { id: 2, full_name: 'kestrel-hq/site', private: false, archived: false },
    { id: 3, full_name: 'kestrel-hq/old', private: true, archived: true },
  ]
  const suspended = { ...organization, id: 2, account_login: 'wren', suspended: true, repositories: [{ id: 4, full_name: 'wren/app', private: true, archived: false }] }
  expect(jobRepositories([{ ...organization, repositories }, suspended], { allow_public_repos: allow, mode })).toEqual(want)
})
