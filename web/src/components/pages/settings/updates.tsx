import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ExternalLink } from 'lucide-react'
import { useEffect, useState } from 'react'
import { $api, errorMessage, isApiError, type Schemas } from '~/api/client'
import { statusQuery } from '~/api/queries'
import { AlertDialog, AlertDialogClose, AlertDialogFooter, AlertDialogPopup, AlertDialogTrigger } from '~/components/ui/alert-dialog'
import { Button } from '~/components/ui/button'
import { Spinner } from '~/components/ui/spinner'
import { formatDate } from '~/lib/format'
import { LoadError, quietAction, SettingRow, SettingRows, SettingsSection, SettingRowsSkeleton } from './parts'
import { SwitchSetting } from './setting-controls'

const updateQuery = $api.queryOptions('get', '/api/update')

/**
 * The Updates section: the running version, the newest release, and the auto
 * update switch. GET /api/update answers 503 when this build has no updater.
 */
export function Updates({ autoUpdate }: { autoUpdate: boolean }) {
  const update = useQuery({
    ...updateQuery,
    retry: (failures, error) => failures < 2 && !isApiError(error, 503) && !isApiError(error, 403),
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
            <LoadError what="the update status" onRetry={() => void update.refetch()} retrying={update.isFetching} />
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

  if (updating) {
    const step = update.state === 'applying' ? 'Installing' : 'Restarting with'
    return (
      <SettingRow group label={`runplane ${update.current}`} description={`${step} ${update.latest}. This page reloads when it is back.`}>
        <Spinner size={16} label="Updating" />
      </SettingRow>
    )
  }

  return (
    <SettingRow group label={`runplane ${update.current}`} description={versionNote(update)}>
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
  return `${update.latest} is available.${released}`
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
  const apply = $api.useMutation('post', '/api/update', {
    onSuccess: () => {
      setOpen(false)
      return client.invalidateQueries({ queryKey: updateQuery.queryKey })
    },
  })

  return (
    <AlertDialog open={open} onOpenChange={setOpen} onOpenChangeComplete={(isOpen) => !isOpen && apply.reset()}>
      <AlertDialogTrigger render={<Button variant="primary" />}>Update now</AlertDialogTrigger>
      <AlertDialogPopup
        title={`Update to ${latest}?`}
        description="runplane downloads the signed release and restarts. This page reconnects when it is back."
      >
        {apply.error && (
          <p role="alert" className="text-s text-error">
            {errorMessage(apply.error)}
          </p>
        )}
        <AlertDialogFooter>
          <AlertDialogClose render={<Button />}>Cancel</AlertDialogClose>
          <Button variant="primary" loading={apply.isPending} onClick={() => apply.mutate({})}>
            Update
          </Button>
        </AlertDialogFooter>
      </AlertDialogPopup>
    </AlertDialog>
  )
}

function NoUpdater() {
  const version = useQuery(statusQuery).data?.version
  return <SettingRow group label={version ? `runplane ${version}` : 'runplane'} description="Updates are not available in this build." />
}
