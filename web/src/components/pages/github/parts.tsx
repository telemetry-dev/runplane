import { useQueryClient } from '@tanstack/react-query'
import { X } from 'lucide-react'
import type { AnchorHTMLAttributes, ReactNode } from 'react'
import { $api, errorMessage } from '~/api/client'
import { githubQuery } from '~/api/queries'
import { StatusIcon } from '~/components/app/status-icon'
import { Button, IconButton } from '~/components/ui/button'
import { Spinner } from '~/components/ui/spinner'
import { cx } from '~/lib/cx'

/**
 * The bordered white list of the designs ("The app asks for", "Installed on").
 * The 1px edge is an overlay, as in DataTable, so row fills do not cover it;
 * the padding gives it its own pixel, as a border would.
 */
export const borderedList = cx(
  'relative overflow-hidden rounded-m bg-neutral-1 p-px',
  'after:pointer-events-none after:absolute after:inset-0 after:rounded-[inherit] after:shadow-[inset_0_0_0_1px_var(--neutral-4)]',
)

/** A row of `borderedList`, with a divider under every row but the last. */
export const borderedRow = 'not-last:border-b not-last:border-neutral-4'

/** A quiet text link to a GitHub page, as "Configure" and "Manage access". It opens a new tab. */
export function ExternalTextLink({ className, children, ...props }: AnchorHTMLAttributes<HTMLAnchorElement>) {
  return (
    <a
      target="_blank"
      rel="noreferrer"
      className={cx(
        'inline-flex shrink-0 items-center gap-xs rounded-xs text-s font-medium whitespace-nowrap text-neutral-7 transition-colors hover:text-neutral-10',
        className,
      )}
      {...props}
    >
      {children}
    </a>
  )
}

/** The `error` that the GitHub callbacks put in the URL when they send the browser back. */
export function CallbackError({ message, onDismiss }: { message: string; onDismiss: () => void }) {
  return (
    <CallbackNotice role="alert" icon={<StatusIcon status="failed" />} title="GitHub did not finish the connection" onDismiss={onDismiss}>
      <p className="text-neutral-8">{message}</p>
    </CallbackNotice>
  )
}

type InstallRequestedProps = {
  /** The installations that runplane knows now. */
  installations: number
  /** Shows "Check again", which syncs the installations with GitHub. Only admins may sync. */
  canSync: boolean
  onDismiss: () => void
}

/**
 * The `requested` callback: a member of an organization who is not its owner
 * installed the app, so GitHub asks an owner to approve the installation.
 */
export function InstallRequested({ installations, canSync, onDismiss }: InstallRequestedProps) {
  const queryClient = useQueryClient()
  const sync = $api.useMutation('post', '/api/github/sync', {
    onSuccess: (status) => {
      queryClient.setQueryData(githubQuery.queryKey, status)
      if (status.installations.length > installations) onDismiss()
    },
  })

  return (
    <CallbackNotice role="status" icon={<StatusIcon status="queued" />} title="Waiting for an organization owner" onDismiss={onDismiss}>
      <p className="text-neutral-8">
        GitHub sent the install request to an owner of the organization. After they approve it, runplane picks up the installation
        within 5 minutes{canSync && ', or at once when you check again'}.
      </p>
      {canSync && (
        <div className="mt-xs flex flex-wrap items-center gap-s">
          <Button loading={sync.isPending} onClick={() => sync.mutate({})}>
            Check again
          </Button>
          {sync.isSuccess && <p className="text-neutral-7">GitHub has no new installation yet.</p>}
          {sync.error && (
            <p role="alert" className="text-error">
              {errorMessage(sync.error)}
            </p>
          )}
        </div>
      )}
    </CallbackNotice>
  )
}

type CallbackNoticeProps = {
  role: 'alert' | 'status'
  icon: ReactNode
  title: string
  onDismiss: () => void
  children: ReactNode
}

function CallbackNotice({ role, icon, title, onDismiss, children }: CallbackNoticeProps) {
  return (
    <div role={role} className="flex items-start gap-s rounded-m bg-neutral-2 py-s pr-s pl-m">
      {/* Centered on the first text line. The title says what the icon shows. */}
      <span aria-hidden className="mt-[1px] flex h-(--line-s) shrink-0 items-center">
        {icon}
      </span>
      <div className="flex min-w-0 flex-1 flex-col gap-xxs pt-[1px] text-s">
        <p className="font-medium text-neutral-10">{title}</p>
        {children}
      </div>
      <IconButton label="Dismiss" onClick={onDismiss} className="-my-xxs">
        <X aria-hidden />
      </IconButton>
    </div>
  )
}

/** Shown in place of page content whose data takes longer than a second to load. */
export function Loading() {
  return (
    <p role="status" className="flex items-center gap-s text-s text-neutral-7">
      <Spinner size={16} />
      Loading…
    </p>
  )
}
