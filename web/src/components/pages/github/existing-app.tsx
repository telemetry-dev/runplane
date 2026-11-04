import { Link } from '@tanstack/react-router'
import { Plus } from 'lucide-react'
import type { Schemas } from '~/api/client'
import { buttonClassName } from '~/components/ui/button'
import { GithubIcon } from '~/components/ui/icons'
import { GitHubAppCard } from './app-card'
import { InstallationList } from './installation-list'

/**
 * Step 2 of the setup once the app exists. Without an installation the app can
 * see no repositories, so the only way on is to install it.
 */
export function ExistingApp({ app, installations }: { app: Schemas['GitHubApp']; installations: Schemas['Installation'][] }) {
  if (installations.length === 0) {
    return (
      <>
        <GitHubAppCard app={app} />
        <div className="flex flex-wrap items-center gap-x-m gap-y-s">
          <a href={app.install_url} className={buttonClassName('primary')}>
            <GithubIcon aria-hidden />
            Install GitHub App
          </a>
          <p className="text-s text-neutral-7">Next, pick the repositories that can use your runners.</p>
        </div>
      </>
    )
  }

  return (
    <>
      <GitHubAppCard app={app} />
      <div className="flex flex-col gap-xs">
        <h2 className="text-s font-medium text-neutral-10">Installed on</h2>
        <InstallationList installations={installations} />
      </div>
      <div className="flex flex-wrap items-center gap-xs">
        <Link to="/setup/first-job" className={buttonClassName('primary')}>
          Continue
        </Link>
        <a href={app.install_url} className={buttonClassName('secondary')}>
          <Plus aria-hidden />
          Install on another account
        </a>
      </div>
    </>
  )
}
