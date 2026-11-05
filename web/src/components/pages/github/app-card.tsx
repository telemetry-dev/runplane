import { Check } from 'lucide-react'
import type { ReactNode } from 'react'
import type { Schemas } from '~/api/client'
import { KeyValue, KeyValueList } from '~/components/app/key-value'
import { CheckIcon } from '~/components/app/status-icon'
import { GithubIcon } from '~/components/ui/icons'
import { Spinner } from '~/components/ui/spinner'
import { formatDate, formatDateTime, formatRelativeTime } from '~/lib/format'
import { useNow } from '~/lib/use-now'
import { summarizeListeners } from './listener'

type GitHubCardProps = {
  title: ReactNode
  detail: ReactNode
  /** A button at the end of the title row, as "Edit on GitHub". It wraps below on narrow screens. */
  action?: ReactNode
  /** Details under a divider, as the listener status. */
  children?: ReactNode
}

/** The card of the "GitHub App" section of Settings · GitHub, with the GitHub mark in a 36px tile. */
export function GitHubCard({ title, detail, action, children }: GitHubCardProps) {
  return (
    <div className="flex flex-col gap-l rounded-m bg-neutral-2 p-l">
      <div className="flex flex-wrap items-center gap-m">
        <span className="grid size-[36px] shrink-0 place-items-center rounded-s bg-neutral-1 text-neutral-10">
          <GithubIcon size={18} aria-hidden />
        </span>
        <div className="flex min-w-0 flex-1 basis-[200px] flex-col">
          <span className="truncate text-s font-medium text-neutral-10">{title}</span>
          <span className="text-xs text-neutral-7">{detail}</span>
        </div>
        {action}
      </div>
      {/* The divider gets its own pixel of padding, as a border would. */}
      {children && <div className="flex flex-col gap-m pt-[calc(var(--space-l)+1px)] shadow-[inset_0_1px_0_var(--neutral-4)]">{children}</div>}
    </div>
  )
}

/** GitHubCard of an app: name, App ID, and created date. */
export function GitHubAppCard({ app, ...props }: Omit<GitHubCardProps, 'title' | 'detail'> & { app: Schemas['GitHubApp'] }) {
  return <GitHubCard title={app.name} detail={`App ID ${app.app_id} · created ${formatDate(app.created_at)}`} {...props} />
}

/** Where the design shows the webhook: the state of the listeners that poll GitHub for jobs. */
export function ListenerStatus({ scaleSets }: { scaleSets: Schemas['ScaleSetStatus'][] }) {
  const now = useNow(1000)
  const { state, total, failing, errors, lastPoll, runnerGroups } = summarizeListeners(scaleSets)

  return (
    <>
      <KeyValueList className="gap-x-xl [&>*]:flex-1">
        <KeyValue label="Listener">
          <span className="flex items-center gap-xs">
            {state === 'connected' && <Check size={14} aria-hidden />}
            {state === 'failing' && <CheckIcon status="warn" size={14} />}
            {state === 'starting' && <Spinner size={14} />}
            {state === 'connected' && 'Connected'}
            {state === 'failing' && `${failing} of ${total} failing`}
            {state === 'starting' && 'Starting'}
            {state === 'off' && <span className="text-neutral-7">Not listening</span>}
          </span>
        </KeyValue>
        <KeyValue label="Last poll">
          {lastPoll ? (
            <time dateTime={lastPoll} title={formatDateTime(lastPoll, { full: true })}>
              {formatRelativeTime(lastPoll, now)}
            </time>
          ) : (
            <span className="text-neutral-7">Never</span>
          )}
        </KeyValue>
        {runnerGroups.length > 0 && (
          <KeyValue label={runnerGroups.length === 1 ? 'Runner group' : 'Runner groups'}>{runnerGroups.join(', ')}</KeyValue>
        )}
      </KeyValueList>
      {errors.length > 0 && (
        <ul aria-label="Listener errors" className="flex flex-col gap-s text-s">
          {errors.map(({ message, scaleSets: sets }) => (
            <li key={message} className="flex min-w-0 flex-col gap-xxs">
              <p className="text-neutral-10">{message}</p>
              <p className="text-xs text-neutral-7">
                {sets.map((set) => `${set.pool_label} · ${set.repository || set.account_login}`).join(', ')}
              </p>
            </li>
          ))}
        </ul>
      )}
    </>
  )
}
