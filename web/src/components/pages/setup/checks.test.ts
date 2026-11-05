import { expect, test } from 'vitest'
import { isSetupCode, modeReason } from './checks'

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
  ['7KQM R4TX W9HB', true],
  ['', false],
])('isSetupCode(%j) is %s', (code, want) => {
  expect(isSetupCode(code)).toBe(want)
})

test('container mode says VM mode is possible only when the server has KVM', () => {
  expect(modeReason({ mode: 'container', kvm: true })).toContain('VM mode is possible')
  expect(modeReason({ mode: 'container', kvm: false })).not.toContain('VM mode is possible')
})
