import { $api } from './client'

/** Every user can read the settings; only admins can change them. */
export const settingsQuery = $api.queryOptions('get', '/api/settings')

/** The DNS record of the domain and the addresses of this server. */
export const domainDNSQuery = $api.queryOptions('get', '/api/settings/dns')
