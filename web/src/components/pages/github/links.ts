import type { Schemas } from '~/api/client'
import { publicReposRun } from '~/api/settings'
import { formatCount } from '~/lib/format'

/**
 * owner/name of the repositories whose jobs runplane takes: not archived, in an
 * installation that is not suspended, and private unless jobs of public repositories run.
 */
export function jobRepositories(
  installations: readonly Schemas['Installation'][],
  settings: Pick<Schemas['Settings'], 'allow_public_repos' | 'public_repos_possible'>,
): string[] {
  const publicRepos = publicReposRun(settings)
  return installations
    .filter((installation) => !installation.suspended)
    .flatMap((installation) => installation.repositories)
    .filter((repository) => !repository.archived && (repository.private || publicRepos))
    .map((repository) => repository.full_name)
}

/** "All repositories · 12" or "3 selected repositories". */
export function repositoryAccess({ repository_selection, repository_count }: Schemas['Installation']): string {
  if (repository_selection === 'all') return `All repositories · ${formatCount(repository_count)}`
  return `${formatCount(repository_count)} selected ${repository_count === 1 ? 'repository' : 'repositories'}`
}
