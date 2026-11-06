import { $api, type Schemas } from './client'

/** Every user can read the settings; only admins can change them. */
export const settingsQuery = $api.queryOptions('get', '/api/settings')

/** The DNS record of the domain and the addresses of this server. */
export const domainDNSQuery = $api.queryOptions('get', '/api/settings/dns')

/** Jobs of public repositories run: the setting is on, and the modes of runplane let them run. */
export function publicReposRun(settings: Pick<Schemas['Settings'], 'allow_public_repos' | 'public_repos_possible'>): boolean {
  return settings.allow_public_repos && settings.public_repos_possible
}
