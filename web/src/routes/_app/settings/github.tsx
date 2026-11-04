import { useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { ExternalLink, Plus, RefreshCw } from 'lucide-react'
import type { ReactNode } from 'react'
import { $api, errorMessage, type Schemas } from '~/api/client'
import { githubQuery, sessionQuery } from '~/api/queries'
import { SectionHeader } from '~/components/app/section-header'
import { GitHubAppCard, GitHubCard, ListenerStatus } from '~/components/pages/github/app-card'
import { callbackSearch } from '~/components/pages/github/callback'
import { DisconnectGitHub } from '~/components/pages/github/disconnect'
import { InstallationList } from '~/components/pages/github/installation-list'
import { JobAccess } from '~/components/pages/github/job-access'
import { CallbackError, InstallRequested, Loading } from '~/components/pages/github/parts'
import { settingsQuery } from '~/components/pages/github/queries'
import { buttonClassName, IconButton } from '~/components/ui/button'
import { GithubIcon } from '~/components/ui/icons'
import { cx } from '~/lib/cx'
import { pageHead } from '~/lib/head'

export const Route = createFileRoute('/_app/settings/github')({
  validateSearch: callbackSearch,
  loader: ({ context: { queryClient } }) =>
    Promise.all([queryClient.ensureQueryData(githubQuery), queryClient.ensureQueryData(settingsQuery)]),
  head: () => pageHead('GitHub settings'),
  pendingComponent: () => (
    <Column>
      <Loading />
    </Column>
  ),
  component: GithubSettings,
})

function GithubSettings() {
  const { error, requested } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { app, installations, scale_sets } = useSuspenseQuery(githubQuery).data
  const settings = useSuspenseQuery(settingsQuery).data
  const isAdmin = useSuspenseQuery(sessionQuery).data?.role === 'admin'
  const dismiss = () => void navigate({ search: {}, replace: true })

  return (
    <Column>
      {error && <CallbackError message={error} onDismiss={dismiss} />}
      {requested && <InstallRequested installations={installations.length} canSync={isAdmin} onDismiss={dismiss} />}
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
          <NotConnected isAdmin={isAdmin} />
        )}
      </section>
      {app && <Installations app={app} installations={installations} isAdmin={isAdmin} />}
      <JobAccess settings={settings} canEdit={isAdmin} />
      {app && isAdmin && <DisconnectGitHub appName={app.name} />}
    </Column>
  )
}

/** The 720px column of the settings design. */
function Column({ children }: { children: ReactNode }) {
  return <div className="flex w-full max-w-[720px] flex-col gap-xxl">{children}</div>
}

type InstallationsProps = {
  app: Schemas['GitHubApp']
  installations: Schemas['Installation'][]
  isAdmin: boolean
}

function Installations({ app, installations, isAdmin }: InstallationsProps) {
  const queryClient = useQueryClient()
  // runplane syncs installations every 5 minutes; this picks up changes made on GitHub now.
  const sync = $api.useMutation('post', '/api/github/sync', {
    onSuccess: (status) => queryClient.setQueryData(githubQuery.queryKey, status),
  })

  return (
    <section className="flex flex-col gap-s">
      <SectionHeader
        title="Installed on"
        action={
          isAdmin && (
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

function NotConnected({ isAdmin }: { isAdmin: boolean }) {
  return (
    <GitHubCard
      title="Not connected"
      detail="runplane needs a GitHub App to take jobs from your repositories."
      action={
        isAdmin && (
          <Link to="/setup/github" className={buttonClassName('primary')}>
            <GithubIcon aria-hidden />
            Connect GitHub
          </Link>
        )
      }
    />
  )
}
