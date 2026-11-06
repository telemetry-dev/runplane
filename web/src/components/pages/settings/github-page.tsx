import { useSuspenseQuery, type QueryClient } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import { ExternalLink, Plus, RefreshCw } from 'lucide-react'
import { errorMessage, type Schemas } from '~/api/client'
import { githubQuery, useSyncGitHub } from '~/api/github'
import { poolsQuery } from '~/api/pools'
import { useCapabilities } from '~/api/session'
import { settingsQuery } from '~/api/settings'
import { SectionHeader } from '~/components/app/section-header'
import { suggestedLabel } from '~/components/pages/first-job/first-job'
import { GitHubAppCard, GitHubCard, ListenerStatus } from '~/components/pages/github/app-card'
import type { CallbackSearch } from '~/components/pages/github/callback'
import { DisconnectGitHub } from '~/components/pages/github/disconnect'
import { InstallationList } from '~/components/pages/github/installation-list'
import { CallbackError, InstallRequested, Loading } from '~/components/pages/github/parts'
import { PullRequestForm, useJobRepositories } from '~/components/pages/github/pull-request-form'
import { buttonClassName, IconButton } from '~/components/ui/button'
import { GithubIcon } from '~/components/ui/icons'
import { cx } from '~/lib/cx'
import { JobAccess } from './job-access'
import { SettingsColumn } from './parts'

export function prefetch(client: QueryClient) {
  return Promise.all([client.ensureQueryData(githubQuery), client.ensureQueryData(settingsQuery), client.ensureQueryData(poolsQuery)])
}

/** Settings · GitHub: the app, its installations, the pull request form, job access, and the disconnect. `search` is what the GitHub callbacks report. */
export function GitHubSettings({ search: { error, requested } }: { search: CallbackSearch }) {
  const navigate = useNavigate()
  const { app, installations, scale_sets } = useSuspenseQuery(githubQuery).data
  const settings = useSuspenseQuery(settingsQuery).data
  const { admin } = useCapabilities()
  const dismiss = () => void navigate({ to: '/settings/github', search: {}, replace: true })

  return (
    <SettingsColumn>
      {error && <CallbackError message={error} onDismiss={dismiss} />}
      {requested && <InstallRequested installations={installations.length} canSync={admin} onDismiss={dismiss} />}
      <section className="flex flex-col gap-s">
        <SectionHeader title="GitHub App" />
        {app ? (
          <GitHubAppCard
            app={app}
            action={
              <a href={app.settings_url} target="_blank" rel="noreferrer" className={buttonClassName('secondary', 'shrink-0')}>
                Edit on GitHub
                <ExternalLink aria-hidden />
                <span className="sr-only"> (opens in a new tab)</span>
              </a>
            }
          >
            <ListenerStatus scaleSets={scale_sets} />
          </GitHubAppCard>
        ) : (
          <NotConnected admin={admin} />
        )}
      </section>
      {app && <Installations app={app} installations={installations} admin={admin} />}
      {app && admin && <PullRequests />}
      <JobAccess settings={settings} admin={admin} />
      {app && admin && <DisconnectGitHub appName={app.name} />}
    </SettingsColumn>
  )
}

export function GitHubSettingsLoading() {
  return (
    <SettingsColumn>
      <Loading />
    </SettingsColumn>
  )
}

type InstallationsProps = {
  app: Schemas['GitHubApp']
  installations: Schemas['Installation'][]
  admin: boolean
}

function Installations({ app, installations, admin }: InstallationsProps) {
  const sync = useSyncGitHub()

  return (
    <section className="flex flex-col gap-s">
      <SectionHeader
        title="Installed on"
        action={
          admin && (
            <div className="flex items-center gap-xs">
              <IconButton label="Sync with GitHub" onClick={() => sync.mutate({})} disabled={sync.isPending}>
                <RefreshCw aria-hidden className={cx(sync.isPending && 'animate-spin')} />
              </IconButton>
              <a href={app.install_url} className={buttonClassName('secondary')}>
                <Plus aria-hidden />
                Install on another account
              </a>
            </div>
          )
        }
      />
      <InstallationList installations={installations} />
      {sync.error && (
        <p role="alert" className="text-s text-error">
          {errorMessage(sync.error)}
        </p>
      )}
    </section>
  )
}

/** "Open a pull request": moves the jobs of one more repository to runplane at any time after the setup. */
function PullRequests() {
  const pools = useSuspenseQuery(poolsQuery).data
  const labels = pools.filter((pool) => pool.enabled).map((pool) => pool.label)
  const label = suggestedLabel(pools)
  const repositories = useJobRepositories()
  if (!label || repositories.length === 0) return null

  return (
    <section className="flex flex-col gap-s">
      <SectionHeader title="Open a pull request" />
      <PullRequestForm labels={labels} defaultLabel={label} />
    </section>
  )
}

function NotConnected({ admin }: { admin: boolean }) {
  return (
    <GitHubCard
      title="Not connected"
      detail="runplane needs a GitHub App to take jobs from your repositories."
      action={
        admin && (
          <Link to="/setup/github" className={buttonClassName('primary')}>
            <GithubIcon aria-hidden />
            Connect GitHub
          </Link>
        )
      }
    />
  )
}
