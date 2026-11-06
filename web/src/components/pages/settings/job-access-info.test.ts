import { expect, test } from 'vitest'
import { publicReposSwitch } from './job-access-info'

const refusal = 'Jobs of public repositories need VM mode on each server, and cheap-vps runs container mode.'
const risk = 'Anyone who opens a pull request could run code on your servers.'

test.each([
  // A server connected in container mode after the admin turned the setting on: the admin can still turn it off.
  ['on, a server waits', true, refusal, true, { disabled: false, description: `${refusal} These servers get no runners until you turn this off.` }],
  ['off, a server is not in VM mode', false, refusal, true, { disabled: true, description: refusal }],
  ['off, each server in VM mode', false, '', true, { disabled: false, description: risk }],
  ['on, for a member', true, '', false, { disabled: true, description: risk }],
] as const)('publicReposSwitch: %s', (_name, on, refusal, admin, want) => {
  expect(publicReposSwitch({ allow_public_repos: on, public_repos_refusal: refusal }, admin)).toEqual(want)
})
