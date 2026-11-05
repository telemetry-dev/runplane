import { expect, test } from 'vitest'
import type { Schemas } from '~/api/client'
import { sortChecks } from './doctor-checks'

type Check = Schemas['Check']

test('sortChecks puts failures, then warnings, then passed checks in the doctor order', () => {
  const check = (id: string, status: Check['status']): Check => ({ id, name: id, status, detail: '', fix: '', command: '' })
  const sorted = sortChecks([check('mode', 'ok'), check('time', 'warn'), check('kernel', 'ok'), check('nft', 'fail'), check('kvm', 'ok')])
  expect(sorted.map((item) => item.id)).toEqual(['nft', 'time', 'mode', 'kernel', 'kvm'])
})
