import type { Schemas } from '~/api/client'

type PublicRepos = Pick<Schemas['Settings'], 'allow_public_repos' | 'public_repos_refusal'>

/**
 * The switch of allow_public_repos, which shows the stored setting. An admin
 * can turn it off at any time, but can turn it on only while each server is
 * in VM mode. While it is on, the servers that the refusal names get no
 * runners.
 */
export function publicReposSwitch({ allow_public_repos: on, public_repos_refusal: refusal }: PublicRepos, admin: boolean) {
  let description = 'Anyone who opens a pull request could run code on your servers.'
  if (refusal) description = on ? `${refusal} These servers get no runners until you turn this off.` : refusal
  return { disabled: !admin || (!on && refusal !== ''), description }
}
