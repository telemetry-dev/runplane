import { useSuspenseQuery, type QueryClient } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import type { Schemas } from '~/api/client'
import { githubQuery } from '~/api/github'
import { poolsQuery } from '~/api/pools'
import { useCapabilities } from '~/api/session'
import { CodeBlock } from '~/components/app/code-block'
import { JobWait, LabelTable, suggestedLabel, workflowDiff } from '~/components/pages/first-job/first-job'
import { ExternalTextLink, Loading } from '~/components/pages/github/parts'
import { PullRequestForm, useJobRepositories } from '~/components/pages/github/pull-request-form'
import { GithubIcon } from '~/components/ui/icons'
import { formatCount } from '~/lib/format'
import { SetupHeader } from './parts'

export function prefetch(client: QueryClient) {
  return Promise.all([client.ensureQueryData(githubQuery), client.ensureQueryData(poolsQuery)])
}

/**
 * Setup step 3, the "Setup · First job" design: the change to a workflow, the
 * pull request form for admins, the labels, and the wait for the first job.
 */
export function FirstJobStep() {
  const navigate = useNavigate()
  const { admin } = useCapabilities()
  const { installations } = useSuspenseQuery(githubQuery).data
  const pools = useSuspenseQuery(poolsQuery).data
  const labels = pools.filter((pool) => pool.enabled)
  const label = suggestedLabel(pools)
  const repositories = useJobRepositories()

  return (
    <>
      <Header />
      <ConnectedAccounts installations={installations} />
      {label && <CodeBlock title=".github/workflows/ci.yml" code={workflowDiff(label)} copyable />}
      {label && admin && repositories.length > 0 && (
        <section className="flex flex-col gap-xs">
          <h2 className="text-s font-medium text-neutral-10">Open a pull request</h2>
          <PullRequestForm labels={[label]} defaultLabel={label} />
        </section>
      )}
      <LabelTable pools={labels} />
      <JobWait onJob={() => void navigate({ to: '/' })} />
    </>
  )
}

export function FirstJobStepLoading() {
  return (
    <>
      <Header />
      <Loading />
    </>
  )
}

function Header() {
  return (
    <SetupHeader title="Run your first job">
      Change one line in any workflow. Jobs with a runplane label run on your servers; everything else stays on GitHub.
    </SetupHeader>
  )
}

/** One row for each account that the app is installed on, as "kestrel-hq connected · 12 repositories". */
function ConnectedAccounts({ installations }: { installations: Schemas['Installation'][] }) {
  if (installations.length === 0) {
    return (
      <div className={accountRow}>
        <GithubIcon size={16} aria-hidden />
        <p className="min-w-0 flex-1 text-s text-neutral-7">No GitHub account is connected yet.</p>
        <Link to="/setup/github" className="shrink-0 rounded-xs text-s font-medium text-neutral-7 transition-colors hover:text-neutral-10">
          Connect GitHub
        </Link>
      </div>
    )
  }
  return (
    <ul aria-label="Connected accounts" className="flex flex-col gap-xs">
      {installations.map((installation) => (
        <li key={installation.id} className={accountRow}>
          <GithubIcon size={16} aria-hidden />
          <p className="flex min-w-0 flex-1 flex-wrap items-baseline gap-x-xs text-s">
            <span className="font-medium text-neutral-10">{installation.account_login}</span>
            <span className="text-neutral-7">
              {installation.suspended ? 'suspended' : 'connected'} · {formatCount(installation.repository_count)}{' '}
              {installation.repository_count === 1 ? 'repository' : 'repositories'}
            </span>
          </p>
          <ExternalTextLink
            href={installation.html_url}
            aria-label={`Manage access of ${installation.account_login} on GitHub (opens in a new tab)`}
          >
            Manage access
          </ExternalTextLink>
        </li>
      ))}
    </ul>
  )
}

const accountRow = 'flex items-center gap-s rounded-s bg-neutral-2 px-m py-s text-neutral-10'
