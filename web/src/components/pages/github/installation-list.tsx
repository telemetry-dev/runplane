import { ExternalLink } from 'lucide-react'
import type { Schemas } from '~/api/client'
import { Avatar } from '~/components/ui/avatar'
import { cx } from '~/lib/cx'
import { repositoryAccess } from './links'
import { borderedList, borderedRow, ExternalTextLink } from './parts'

/**
 * The "Installed on" list of Settings · GitHub. Below 560px of list width the
 * repository column moves under the account name.
 */
export function InstallationList({ installations }: { installations: Schemas['Installation'][] }) {
  return (
    <ul aria-label="Installed on" className={cx(borderedList, '@container')}>
      {installations.map((installation) => {
        const access = installation.suspended ? 'Suspended' : repositoryAccess(installation)
        return (
          <li key={installation.id} className={cx('flex items-center gap-m px-m py-s', borderedRow)}>
            <Avatar name={installation.account_login} size="l" />
            <div className="flex min-w-0 flex-1 flex-col">
              <span className="truncate text-s font-medium text-neutral-10">{installation.account_login}</span>
              <span className="truncate text-xs text-neutral-7">
                {installation.account_type === 'Organization' ? 'Organization' : 'Personal account'}
                <span className="@min-[560px]:hidden"> · {access}</span>
              </span>
            </div>
            <span className="hidden w-[200px] shrink-0 truncate text-s text-neutral-8 @min-[560px]:block">{access}</span>
            <ExternalTextLink
              href={installation.html_url}
              aria-label={`Configure ${installation.account_login} on GitHub (opens in a new tab)`}
            >
              Configure
              <ExternalLink size={14} aria-hidden />
            </ExternalTextLink>
          </li>
        )
      })}
      {installations.length === 0 && (
        <li className="px-m py-xl text-center text-s text-neutral-7">The app is not installed on any account yet.</li>
      )}
    </ul>
  )
}
