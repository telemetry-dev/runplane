import type { Schemas } from '~/api/client'
import { formatCount } from '~/lib/format'

/** "All repositories · 12" or "3 selected repositories". */
export function repositoryAccess({ repository_selection, repository_count }: Schemas['Installation']): string {
  if (repository_selection === 'all') return `All repositories · ${formatCount(repository_count)}`
  return `${formatCount(repository_count)} selected ${repository_count === 1 ? 'repository' : 'repositories'}`
}
