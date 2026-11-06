import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Download } from 'lucide-react'
import { useState, type FormEvent } from 'react'
import { backupsQuery } from '~/api/backups'
import { $api, errorMessage, type Schemas } from '~/api/client'
import { useFormProblems } from '~/api/form-problems'
import { limits } from '~/api/limits'
import { useSaveSettings } from '~/api/settings'
import {
  DataTable,
  DataTableCell,
  DataTableEmpty,
  DataTableHead,
  DataTableHeader,
  DataTableIconHead,
  DataTableRow,
  useFittingColumns,
} from '~/components/app/data-table'
import { ErrorHint } from '~/components/app/error-hint'
import { LoadErrorRow } from '~/components/app/load-error'
import { None } from '~/components/app/none'
import { SkeletonCell, SkeletonRows } from '~/components/app/skeleton-rows'
import { StatusIcon } from '~/components/app/status-icon'
import { Button, IconButton } from '~/components/ui/button'
import { Field, Input } from '~/components/ui/field'
import { Form } from '~/components/ui/form'
import { toastManager } from '~/components/ui/toast'
import { Tooltip } from '~/components/ui/tooltip'
import { formatBytes, formatDateTime } from '~/lib/format'
import { backupStatus, storedOn } from './backup-info'
import { CardTitle, SettingRows, SettingsCard, SettingsSection } from './parts'
import { showSaved } from './save'
import { NumberSetting, SwitchSetting } from './setting-controls'

/** The Backups section: the daily backup, how many to keep, the S3 upload, and the list of backups. */
export function Backups({ settings }: { settings: Schemas['Settings'] }) {
  const client = useQueryClient()
  const create = $api.useMutation('post', '/api/backups', {
    onSuccess: (backup) =>
      client.setQueryData<Schemas['Backup'][]>(backupsQuery.queryKey, (list) => [
        backup,
        ...(list ?? []).filter((item) => item.id !== backup.id),
      ]),
    onError: (error) => toastManager.add({ title: 'Could not start a backup', description: errorMessage(error) }),
  })

  return (
    <SettingsSection
      title="Backups"
      action={
        <Button loading={create.isPending} onClick={() => create.mutate({})}>
          Back up now
        </Button>
      }
    >
      <SettingRows>
        <SwitchSetting
          name="backup_enabled"
          label="Back up every day"
          description={`At ${settings.backup_time} UTC runplane saves its database on this server. A restore also needs ${settings.secret_key_path}, which backups do not contain: keep a copy of it in a safe place.`}
          checked={settings.backup_enabled}
        />
        <NumberSetting
          label="Backups to keep"
          description="Older backup files on this server are deleted."
          value={settings.backup_keep}
          toPatch={(backup_keep) => ({ backup_keep })}
          limit={limits.SettingsPatch.backup_keep}
        />
      </SettingRows>
      <div className="flex flex-col gap-l">
        <S3Card s3={settings.backup_s3} />
        <BackupTable />
      </div>
    </SettingsSection>
  )
}

const s3Fields = [
  { name: 'endpoint', label: 'Endpoint', placeholder: 'AWS S3' },
  { name: 'region', label: 'Region' },
  { name: 'bucket', label: 'Bucket' },
  { name: 'prefix', label: 'Prefix' },
  { name: 'access_key', label: 'Access key' },
] as const

type S3Key = (typeof s3Fields)[number]['name']

/** "Upload to S3": the bucket settings, saved together. The secret key is write-only. */
function S3Card({ s3 }: { s3: Schemas['BackupS3'] }) {
  const save = useSaveSettings()
  const [draft, setDraft] = useState<Partial<Record<S3Key, string>>>({})
  const [secret, setSecret] = useState('')
  const value = (name: S3Key) => draft[name] ?? s3[name]
  const dirty = secret !== '' || s3Fields.some(({ name }) => value(name) !== s3[name])

  function submit(event: FormEvent) {
    event.preventDefault()
    const fields = Object.fromEntries(s3Fields.map(({ name }) => [name, value(name)])) as Record<S3Key, string>
    save.mutate(
      { body: { backup_s3: { ...fields, ...(secret && { secret_key: secret }) } } },
      {
        onSuccess: () => {
          setDraft({})
          setSecret('')
          showSaved()
        },
      },
    )
  }

  // The API reports "body.backup_s3.bucket"; the fields are named "bucket".
  const problems = useFormProblems(save.error, { prefix: 'backup_s3.' })

  return (
    <SettingsCard className="gap-l">
      <CardTitle title="Upload to S3">Optional. Without a bucket, backups stay on this server only.</CardTitle>
      <Form errors={problems.fields} onSubmit={submit} className="gap-l">
        <div className="grid grid-cols-1 gap-x-l gap-y-m @min-[480px]:grid-cols-2">
          {s3Fields.map(({ name, label, ...field }) => (
            <Field key={name} name={name} label={label}>
              <Input
                value={value(name)}
                onValueChange={(text) => setDraft((old) => ({ ...old, [name]: text }))}
                placeholder={'placeholder' in field ? field.placeholder : undefined}
                autoComplete="off"
                spellCheck={false}
              />
            </Field>
          ))}
          <Field name="secret_key" label="Secret key">
            <Input
              type="password"
              value={secret}
              onValueChange={setSecret}
              placeholder={s3.secret_key_set ? 'Saved · type to replace' : undefined}
              autoComplete="new-password"
            />
          </Field>
        </div>
        <div className="flex items-center justify-end gap-m">
          {problems.alert && (
            <p role="alert" className="min-w-0 text-s text-error">
              {problems.alert}
            </p>
          )}
          <Button type="submit" disabled={!dirty} loading={save.isPending}>
            Save
          </Button>
        </div>
      </Form>
    </SettingsCard>
  )
}

/** The backups, newest first. */
function BackupTable() {
  const backups = useQuery(backupsQuery)
  // A narrow table drops Stored on, then Size.
  const { ref, shown, columns } = useFittingColumns({
    status: 16,
    created: 'grow',
    size: { width: 80, drop: 2 },
    place: { width: 220, drop: 1 },
    download: 24,
  })

  return (
    <div ref={ref}>
      <DataTable aria-label="Backups" columns={columns}>
        <DataTableHeader>
          <DataTableIconHead label="Status" />
          <DataTableHead>Created</DataTableHead>
          {shown.size && <DataTableHead align="end">Size</DataTableHead>}
          {shown.place && <DataTableHead>Stored on</DataTableHead>}
          <DataTableHead>
            <span className="sr-only">Download</span>
          </DataTableHead>
        </DataTableHeader>
        {backups.data ? (
          backups.data.map((backup) => <BackupRow key={backup.id} backup={backup} shown={shown} />)
        ) : backups.isError ? (
          <LoadErrorRow what="the backups" error={backups.error} onRetry={() => void backups.refetch()} retrying={backups.isFetching} />
        ) : (
          <SkeletonRows label="Loading the backups">
            <SkeletonCell shape="icon" />
            <SkeletonCell lines={[96]} />
            {shown.size && <SkeletonCell lines={[48]} align="end" />}
            {shown.place && <SkeletonCell lines={[112]} />}
            <SkeletonCell />
          </SkeletonRows>
        )}
        {backups.data?.length === 0 && <DataTableEmpty>No backups yet.</DataTableEmpty>}
      </DataTable>
    </div>
  )
}

type Shown = Record<'size' | 'place', boolean>

function BackupRow({ backup, shown }: { backup: Schemas['Backup']; shown: Shown }) {
  const created = formatDateTime(backup.created_at)
  const status = backupStatus(backup)
  const place = storedOn(backup)
  const uploadFailed = backup.status === 'failed' && backup.available

  return (
    <DataTableRow>
      <DataTableCell>
        {status === 'failed' && !uploadFailed ? (
          <ErrorHint error={backup.error}>
            <StatusIcon status={status} />
          </ErrorHint>
        ) : (
          <StatusIcon status={status} />
        )}
      </DataTableCell>
      <DataTableCell>
        <Tooltip content={formatDateTime(backup.created_at, { full: true })}>
          <time dateTime={backup.created_at} className="truncate text-neutral-10">
            {created}
          </time>
        </Tooltip>
      </DataTableCell>
      {shown.size && (
        <DataTableCell align="end" className="tabular-nums">
          {backup.size_bytes > 0 ? formatBytes(backup.size_bytes) : <None />}
        </DataTableCell>
      )}
      {shown.place && (
        <DataTableCell>
          {uploadFailed ? (
            <ErrorHint error={backup.error}>
              <span className="truncate">{place}</span>
            </ErrorHint>
          ) : (
            <span className="truncate">{place || <None />}</span>
          )}
        </DataTableCell>
      )}
      <DataTableCell align="end">
        {backup.available ? (
          <IconButton
            label={`Download backup from ${created}`}
            nativeButton={false}
            render={<a href={`/api/backups/${encodeURIComponent(backup.id)}/download`} download />}
            className="-m-xs"
          >
            <Download aria-hidden />
          </IconButton>
        ) : (
          <IconButton label={`Download backup from ${created}`} disabled className="-m-xs">
            <Download aria-hidden />
          </IconButton>
        )}
      </DataTableCell>
    </DataTableRow>
  )
}
