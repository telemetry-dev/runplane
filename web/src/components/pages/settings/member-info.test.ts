import { expect, test } from 'vitest'
import { roleSummary } from './member-info'

test.each([
  [
    'member',
    { rerun_jobs: true, stop_sandboxes: true, open_shells: true, admin: false },
    'Members can re-run jobs, stop runners, and open debug shells, but cannot change settings.',
  ],
  ['admin', { rerun_jobs: true, stop_sandboxes: true, open_shells: true, admin: true }, 'Admins can re-run jobs, stop runners, open debug shells, and change settings.'],
  ['member', { rerun_jobs: true, stop_sandboxes: false, open_shells: false, admin: false }, 'Members can re-run jobs, but cannot stop runners, open debug shells, or change settings.'],
  ['member', { rerun_jobs: false, stop_sandboxes: false, open_shells: false, admin: false }, 'Members cannot re-run jobs, stop runners, open debug shells, or change settings.'],
] as const)('roleSummary(%s, %j)', (role, capabilities, want) => {
  expect(roleSummary(role, capabilities)).toBe(want)
})
