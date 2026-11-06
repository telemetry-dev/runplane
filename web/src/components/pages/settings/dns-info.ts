import type { Schemas } from '~/api/client'

type DomainDNS = Schemas['DomainDNS']

/** The record that points a domain to this server: A for the first address, AAAA if it is IPv6. The API lists IPv4 first. */
export function serverRecord(addresses: readonly string[]): { type: 'A' | 'AAAA'; value: string } | undefined {
  const value = addresses[0]
  if (!value) return undefined
  return { type: value.includes(':') ? 'AAAA' : 'A', value }
}

export type DNSStatus = {
  tone: 'ok' | 'error' | 'neutral'
  /** Short, for the right side of the row. */
  text: string
  /** What to do, for the row description. */
  help: string
}

/** How often the page asks again for the record while it is wrong. */
export const dnsPollMs = 10_000

/** The page asks again while the record is missing or wrong, or the lookup failed: the states with the error tone. */
export function dnsPolls(dns: DomainDNS): boolean {
  return dnsStatus(dns).tone === 'error'
}

/** The state of the DNS record of the domain. */
export function dnsStatus(dns: DomainDNS): DNSStatus {
  switch (dns.state) {
    case 'ok':
      return { tone: 'ok', text: 'Points to this server', help: 'Let’s Encrypt connects to this server through the record.' }
    case 'missing':
      return {
        tone: 'error',
        text: 'No A or AAAA record',
        help: `Add the record at your DNS provider. runplane looks for it every ${dnsPollMs / 1000} seconds.`,
      }
    case 'other': {
      const wrong = dns.records.filter((record) => !dns.server_addresses.includes(record))
      return {
        tone: 'error',
        text: `Points to ${wrong.join(', ')}`,
        help: 'Change the record at your DNS provider. If a proxy such as Cloudflare is on for the record, turn it off.',
      }
    }
    case 'unknown':
      return {
        tone: 'neutral',
        text: `Points to ${dns.records.join(', ')}`,
        help: 'runplane cannot find the public IP address of this server. Make sure that the record points to it.',
      }
    case 'error':
      return { tone: 'error', text: 'The DNS lookup failed', help: dns.error }
    case 'none':
      return { tone: 'neutral', text: '', help: '' }
  }
}
