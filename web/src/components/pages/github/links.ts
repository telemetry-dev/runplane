import type { Schemas } from '~/api/client'
import { formatCount } from '~/lib/format'

/** owner/name of the repositories whose jobs runplane takes. */
export function jobRepositories(installations: readonly Schemas['Installation'][]): string[] {
  return installations
    .flatMap((installation) => installation.repositories)
    .filter((repository) => repository.takes_jobs)
    .map((repository) => repository.full_name)
}

/** "All repositories · 12" or "3 selected repositories". */
export function repositoryAccess({ repository_selection, repository_count }: Schemas['Installation']): string {
  if (repository_selection === 'all') return `All repositories · ${formatCount(repository_count)}`
  return `${formatCount(repository_count)} selected ${repository_count === 1 ? 'repository' : 'repositories'}`
}
