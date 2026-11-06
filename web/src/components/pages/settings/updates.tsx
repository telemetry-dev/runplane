import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ExternalLink } from 'lucide-react'
import { useEffect, useState } from 'react'
import { api, isApiError, retryQuery, type Schemas } from '~/api/client'
import { statusQuery } from '~/api/session'
import { updateQuery } from '~/api/update'
import { ConfirmDialog } from '~/components/app/confirm-dialog'
import { LoadError } from '~/components/app/load-error'
import { Button } from '~/components/ui/button'
import { Spinner } from '~/components/ui/spinner'
import { formatDate, formatVersion } from '~/lib/format'
import { quietAction, SettingRow, SettingRows, SettingsSection, SettingRowsSkeleton } from './parts'
import { SwitchSetting } from './setting-controls'

/** The Updates section: the running version, the newest release, and the auto update switch. */
export function Updates({ autoUpdate }: { autoUpdate: boolean }) {
  const update = useQuery({
    ...updateQuery,
    // A build without an updater answers 503, which the retry rule would retry.
    retry: (failures, error) => !isApiError(error, 503) && retryQuery(failures, error),
    // An update runs without events; the state moves from applying to restarting.
    refetchInterval: (query) => (query.state.data && query.state.data.state !== 'idle' ? 2_000 : false),
  })
  const unavailable = isApiError(update.error, 503)

  return (
    <SettingsSection title="Updates">
      {update.isPending ? (
        <SettingRowsSkeleton control="switch" />
      ) : (
        <SettingRows>
          {update.data ? (
            <VersionRow update={update.data} />
          ) : unavailable ? (
            <NoUpdater />
          ) : (
            <LoadError what="the update status" error={update.error} onRetry={() => void update.refetch()} retrying={update.isFetching} />
          )}
          <SwitchSetting
            name="auto_update"
            label="Update automatically"
            description="runplane installs each signed release and restarts."
            checked={autoUpdate}
            disabled={unavailable}
          />
        </SettingRows>
      )}
    </SettingsSection>
  )
}

function VersionRow({ update }: { update: Schemas['UpdateStatus'] }) {
  const updating = update.state !== 'idle'
  useReloadOnNewVersion(update)

  const label = `runplane ${formatVersion(update.current)}`
  if (updating) {
    const step = update.state === 'applying' ? 'Installing' : 'Restarting with'
    return (
      <SettingRow group label={label} description={`${step} ${formatVersion(update.latest)}. This page reloads when it is back.`}>
        <Spinner size={16} label="Updating" />
      </SettingRow>
    )
  }

  return (
    <SettingRow group label={label} description={versionNote(update)}>
      <div className="flex items-center gap-l">
        {update.url && (
          <a href={update.url} target="_blank" rel="noreferrer" className={quietAction}>
            Release notes
            <ExternalLink aria-hidden />
            <span className="sr-only"> (opens in a new tab)</span>
          </a>
        )}
        {update.available && <UpdateNow latest={update.latest} />}
      </div>
    </SettingRow>
  )
}

function versionNote(update: Schemas['UpdateStatus']): string {
  // The release repository has no release yet, as for a build from source.
  if (!update.latest) return 'No release published.'
  if (!update.available) return 'Up to date.'
  const released = update.published_at ? ` Released ${formatDate(update.published_at)}.` : ''
  return `${formatVersion(update.latest)} is available.${released}`
}

/** Reloads the page once runplane answers with the version that an update installed, so the new dashboard loads. */
function useReloadOnNewVersion(update: Schemas['UpdateStatus']) {
  const [from, setFrom] = useState<string>()
  if (update.state !== 'idle' && from === undefined) setFrom(update.current)
  useEffect(() => {
    if (from !== undefined && update.current !== from) window.location.reload()
  }, [from, update.current])
}

function UpdateNow({ latest }: { latest: string }) {
  const client = useQueryClient()
  const [open, setOpen] = useState(false)
  return (
    <>
      <Button variant="primary" onClick={() => setOpen(true)}>
        Update now
      </Button>
      <ConfirmDialog
        open={open}
        onOpenChange={setOpen}
        title={`Update to ${formatVersion(latest)}?`}
        description="runplane downloads the signed release and restarts. This page reconnects when it is back."
        action="Update"
        variant="primary"
        onConfirm={async () => {
          await api.POST('/api/update')
          void client.invalidateQueries({ queryKey: updateQuery.queryKey })
        }}
      />
    </>
  )
}

function NoUpdater() {
  const version = useQuery(statusQuery).data?.version
  return <SettingRow group label={version ? `runplane ${formatVersion(version)}` : 'runplane'} description="Updates are not available in this build." />
}
