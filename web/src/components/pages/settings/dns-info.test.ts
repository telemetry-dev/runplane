import { expect, test } from 'vitest'
import type { Schemas } from '~/api/client'
import { dnsStatus, serverRecord } from './dns-info'

function dns(fields: Partial<Schemas['DomainDNS']>): Schemas['DomainDNS'] {
  return {
    domain: 'ci.example.com',
    state: 'ok',
    records: ['203.0.113.7'],
    server_addresses: ['203.0.113.7', '2001:db8::7'],
    error: '',
    ...fields,
  }
}

test('the record type follows the first server address', () => {
  expect(serverRecord(['203.0.113.7', '2001:db8::7'])).toEqual({ type: 'A', value: '203.0.113.7' })
  expect(serverRecord(['2001:db8::7'])).toEqual({ type: 'AAAA', value: '2001:db8::7' })
  expect(serverRecord([])).toBeUndefined()
})

test('a wrong record names only the addresses that are not this server', () => {
  const status = dnsStatus(dns({ state: 'other', records: ['203.0.113.7', '2606:4700::1'] }))
  expect(status.tone).toBe('error')
  expect(status.text).toBe('Points to 2606:4700::1')
})

test('without a known server address, the records show without a verdict', () => {
  const status = dnsStatus(dns({ state: 'unknown', records: ['198.51.100.4'], server_addresses: [] }))
  expect(status.tone).toBe('neutral')
  expect(status.text).toBe('Points to 198.51.100.4')
})
