import { expect, test } from 'vitest'
import type { Schemas } from '~/api/client'
import { isSetupCode, listedChecks, modeReason } from './checks'

// The host check runs only for codes that pass isSetupCode.
test.each([
  ['7KQM-R4TX-W9HB', true],
  ['7kqm-r4tx-w9hb', true],
  ['7KQMR4TXW9HB', true],
  ['7KQM-R4TX-W9H', false],
  ['7KQM-R4TX-W9HBX', false],
  // I, O, 0, and 1 are not in the alphabet.
  ['7KQM-R4TX-W9H1', false],
  ['7KQM-R4TX-W9HO', false],
  ['7KQM R4TX W9HB', false],
  ['', false],
])('isSetupCode(%j) is %s', (code, want) => {
  expect(isSetupCode(code)).toBe(want)
})

function check(id: string, status: Schemas['Check']['status']): Schemas['Check'] {
  return { id, name: id, status, detail: '', fix: '', command: '' }
}

test('failed checks come first, then warnings, then passed ones, each in API order; the mode check is left out', () => {
  const checks = [
    check('os', 'ok'),
    check('time', 'warn'),
    check('mode', 'ok'),
    check('ip_forward', 'fail'),
    check('kernel', 'ok'),
    check('disk', 'warn'),
    check('kvm', 'fail'),
  ]
  expect(listedChecks(checks).map((c) => c.id)).toEqual(['ip_forward', 'kvm', 'time', 'disk', 'os', 'kernel'])
})

test('container mode says VM mode is possible only when the server has KVM', () => {
  expect(modeReason({ mode: 'container', kvm: true })).toContain('VM mode is possible')
  expect(modeReason({ mode: 'container', kvm: false })).not.toContain('VM mode is possible')
})
