import { Collapsible } from '@base-ui/react/collapsible'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ChevronRight, Ellipsis, RefreshCw, Star, Trash2 } from 'lucide-react'
import { useState } from 'react'
import { $api, errorMessage, type Schemas } from '~/api/client'
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
import { StatusIcon } from '~/components/app/status-icon'
import { AlertDialog, AlertDialogClose, AlertDialogFooter, AlertDialogPopup } from '~/components/ui/alert-dialog'
import { Button, IconButton } from '~/components/ui/button'
import { Field, Input } from '~/components/ui/field'
import { Form } from '~/components/ui/form'
import { Menu, MenuItem, MenuPopup, MenuSeparator, MenuTrigger } from '~/components/ui/menu'
import { toastManager } from '~/components/ui/toast'
import { formatBytes } from '~/lib/format'
import { cx } from '~/lib/cx'
import { Badge, Bar, CardTitle, ErrorHint, LoadError, SettingsCard, SettingsSection } from './parts'
import { buildLabel, buildStatus, buildSummary, usedBy } from './runner-info'
import { useFormErrors } from './settings-api'

type Image = Schemas['Image']

/** GET /api/images. image.state events refetch it. */
const imagesQuery = $api.queryOptions('get', '/api/images')

/** The Images section: every image with its state on each server, and the form to add one. */
export function Images({ admin }: { admin: boolean }) {
  const images = useQuery(imagesQuery)
  // A narrow table drops Servers, then Used by.
  const { ref, shown, columns } = useFittingColumns({
    toggle: 16,
    image: 'grow',
    usedBy: { width: 96, drop: 2 },
    servers: { width: 180, drop: 1 },
    menu: 24,
  })

  return (
    <SettingsSection title="Images" table>
      <div ref={ref} className="flex flex-col gap-l">
        <DataTable aria-label="Images" columns={columns}>
          <DataTableHeader>
            <DataTableIconHead label="Details" />
            <DataTableHead>Image</DataTableHead>
            {shown.usedBy && <DataTableHead>Used by</DataTableHead>}
            {shown.servers && <DataTableHead>Servers</DataTableHead>}
            <DataTableHead>
              <span className="sr-only">Actions</span>
            </DataTableHead>
          </DataTableHeader>
          {images.data ? (
            images.data.map((image) => <ImageRows key={image.id} image={image} admin={admin} shown={shown} />)
          ) : images.isError ? (
            <div role="row" className="px-m">
              <div role="cell">
                <LoadError what="images" onRetry={() => void images.refetch()} retrying={images.isFetching} />
              </div>
            </div>
          ) : (
            <SkeletonRows shown={shown} />
          )}
          {images.data?.length === 0 && <DataTableEmpty>No images yet.</DataTableEmpty>}
        </DataTable>
        {admin && <AddImage />}
      </div>
    </SettingsSection>
  )
}

type Shown = Record<'usedBy' | 'servers', boolean>

type ImageRowsProps = { image: Image; admin: boolean; shown: Shown }

/**
 * An image row, and under it the servers when open. An image starts open while
 * a server pulls or builds it, or when a server failed; waiting servers alone
 * keep it closed.
 */
function ImageRows({ image, admin, shown }: ImageRowsProps) {
  const [open, setOpen] = useState(() => image.builds.some((build) => build.status !== 'ready' && build.status !== 'pending'))

  return (
    <Collapsible.Root
      open={open}
      onOpenChange={setOpen}
      role="rowgroup"
      // The divider gets its own pixel of padding, as a border would.
      className="group/image not-last:pb-px not-last:shadow-[inset_0_-1px_0_var(--neutral-4)]"
    >
      {/* The wrapper keeps the row from drawing the table's line between it and its panel. */}
      <div>
        <DataTableRow>
          <DataTableCell>
            <Collapsible.Trigger
              aria-label={`Servers of ${image.name}`}
              disabled={image.builds.length === 0}
              className="-m-xxs grid place-items-center rounded-xs p-xxs text-neutral-7 transition-colors hover:text-neutral-10 data-disabled:text-neutral-5"
            >
              <ChevronRight
                size={16}
                aria-hidden
                className="transition-[rotate] duration-(--motion-large-duration) ease-large group-data-open/image:rotate-90"
              />
            </Collapsible.Trigger>
          </DataTableCell>
          <DataTableCell className="flex-col items-start gap-0">
            <span className="flex max-w-full items-center gap-xs">
              <span className="truncate font-medium text-neutral-10">{image.name}</span>
              {image.is_default && <Badge>Default</Badge>}
            </span>
            <span className="max-w-full truncate font-code text-xs text-neutral-7">{image.ref}</span>
          </DataTableCell>
          {shown.usedBy && (
            <DataTableCell>
              <span className={cx(image.pool_count === 0 && 'text-neutral-7')}>{usedBy(image.pool_count)}</span>
            </DataTableCell>
          )}
          {shown.servers && (
            <DataTableCell>
              <span className="truncate">{buildSummary(image.builds) || <span className="text-neutral-7">No servers</span>}</span>
            </DataTableCell>
          )}
          <DataTableCell align="end">{admin && <ImageMenu image={image} />}</DataTableCell>
        </DataTableRow>
      </div>
      <Collapsible.Panel
        role="row"
        className={cx(
          'h-(--collapsible-panel-height) overflow-hidden transition-[height,opacity] duration-(--motion-large-duration) ease-large',
          'data-starting-style:h-0 data-starting-style:opacity-0 data-ending-style:h-0 data-ending-style:opacity-0',
        )}
      >
        <div role="cell" className="pr-m pb-m pl-[40px]">
          <ul aria-label={`Servers of ${image.name}`} className="flex flex-col rounded-s bg-neutral-2 py-xxs">
            {image.builds.map((build) => (
              <li key={build.server_id} className="flex items-center gap-m px-s py-xs text-s">
                <ErrorHint error={build.status === 'failed' ? build.error : ''}>
                  <StatusIcon status={buildStatus(build.status)} />
                </ErrorHint>
                <span className="min-w-0 flex-1 truncate text-neutral-10">{build.server_name}</span>
                <span className="w-[56px] shrink-0 text-neutral-7">{build.arch || '—'}</span>
                <span className="w-[124px] shrink-0 text-neutral-8">{buildLabel(build.status)}</span>
                <span className="w-[72px] shrink-0 text-right text-neutral-8 tabular-nums">
                  {build.size_bytes > 0 ? formatBytes(build.size_bytes) : '—'}
                </span>
              </li>
            ))}
          </ul>
        </div>
      </Collapsible.Panel>
    </Collapsible.Root>
  )
}

function ImageMenu({ image }: { image: Image }) {
  const client = useQueryClient()
  const [removing, setRemoving] = useState(false)
  const refresh = () => client.invalidateQueries({ queryKey: imagesQuery.queryKey })
  const failed = (action: string) => (error: unknown) =>
    toastManager.add({ title: `Could not ${action} ${image.name}`, description: errorMessage(error) })

  const makeDefault = $api.useMutation('patch', '/api/images/{id}', { onSuccess: refresh, onError: failed('make the default') })
  const pull = $api.useMutation('post', '/api/images/{id}/refresh', {
    onSuccess: () => {
      toastManager.add({ title: `Pulling ${image.name} again` })
      return refresh()
    },
    onError: failed('pull'),
  })
  const path = { params: { path: { id: image.id } } }

  return (
    <>
      <Menu>
        <MenuTrigger render={<IconButton label={`${image.name} actions`} className="-m-xs" />}>
          <Ellipsis aria-hidden />
        </MenuTrigger>
        <MenuPopup align="end">
          {!image.is_default && (
            <MenuItem icon={<Star />} onClick={() => makeDefault.mutate({ ...path, body: { is_default: true } })}>
              Make default
            </MenuItem>
          )}
          <MenuItem icon={<RefreshCw />} onClick={() => pull.mutate(path)}>
            Pull again
          </MenuItem>
          <MenuSeparator />
          {/* The API refuses to remove the default image. */}
          <MenuItem icon={<Trash2 />} danger disabled={image.is_default} onClick={() => setRemoving(true)}>
            Remove
          </MenuItem>
        </MenuPopup>
      </Menu>
      <RemoveImage image={image} open={removing} onOpenChange={setRemoving} />
    </>
  )
}

function RemoveImage({ image, open, onOpenChange }: { image: Image; open: boolean; onOpenChange: (open: boolean) => void }) {
  const client = useQueryClient()
  const remove = $api.useMutation('delete', '/api/images/{id}', { onSuccess: () => onOpenChange(false) })

  return (
    <AlertDialog
      open={open}
      onOpenChange={onOpenChange}
      onOpenChangeComplete={(isOpen) => {
        if (isOpen) return
        // Drop the row only after the close animation; the dialog lives in it.
        if (remove.isSuccess) {
          client.setQueryData<Image[]>(imagesQuery.queryKey, (list) => list?.filter((item) => item.id !== image.id))
          void client.invalidateQueries({ queryKey: ['get', '/api/pools'] })
        }
        remove.reset()
      }}
    >
      <AlertDialogPopup
        title={`Remove ${image.name}?`}
        description="Each server deletes the image. Runner labels that use it change to the default image."
      >
        {remove.error && (
          <p role="alert" className="text-s text-error">
            {errorMessage(remove.error)}
          </p>
        )}
        <AlertDialogFooter>
          <AlertDialogClose render={<Button />}>Cancel</AlertDialogClose>
          <Button variant="danger" loading={remove.isPending} onClick={() => remove.mutate({ params: { path: { id: image.id } } })}>
            Remove image
          </Button>
        </AlertDialogFooter>
      </AlertDialogPopup>
    </AlertDialog>
  )
}

/** The "Add image" card. Each server starts to pull the new image at once. */
function AddImage() {
  const client = useQueryClient()
  const [name, setName] = useState('')
  const [ref, setRef] = useState('')
  // A new key gives an empty form that has not been submitted, so the cleared fields show no "required" error.
  const [formKey, setFormKey] = useState(0)
  const add = $api.useMutation('post', '/api/images', {
    onSuccess: (image) => {
      client.setQueryData<Image[]>(imagesQuery.queryKey, (list) => [...(list ?? []).filter((item) => item.id !== image.id), image])
      setName('')
      setRef('')
      setFormKey((key) => key + 1)
      toastManager.add({ title: `${image.name} added` })
    },
  })
  const errors = useFormErrors(add.error)

  return (
    <SettingsCard className="gap-m">
      <CardTitle title="Add image">Build it FROM a runplane base image. Each server pulls it once.</CardTitle>
      <Form
        key={formKey}
        errors={errors.fields}
        onFormSubmit={() => add.mutate({ body: { name: name.trim(), ref: ref.trim() } })}
        className="gap-s"
      >
        <div className="flex flex-col gap-s @min-[480px]:flex-row @min-[480px]:items-start">
          <Field name="name" label="Name" className="@min-[480px]:w-[180px] @min-[480px]:shrink-0">
            <Input
              value={name}
              onValueChange={setName}
              placeholder="android-sdk"
              autoComplete="off"
              spellCheck={false}
              required
              maxLength={64}
            />
          </Field>
          <Field name="ref" label="OCI reference" className="min-w-0 flex-1">
            <Input
              value={ref}
              onValueChange={setRef}
              placeholder="ghcr.io/your-org/your-runner-image:tag"
              autoComplete="off"
              spellCheck={false}
              required
              maxLength={512}
            />
          </Field>
          {/* Level with the inputs: below the label line and the field gap. */}
          <Button type="submit" loading={add.isPending} className="self-start @min-[480px]:mt-[calc(var(--line-s)+var(--space-xs))]">
            Add image
          </Button>
        </div>
        {errors.other && (
          <p role="alert" className="text-s text-error">
            {errors.other}
          </p>
        )}
      </Form>
    </SettingsCard>
  )
}

function SkeletonRows({ shown }: { shown: Shown }) {
  return Array.from({ length: 2 }, (_, index) => (
    <DataTableRow key={index}>
      <DataTableCell />
      <DataTableCell className="flex-col items-start gap-0">
        <Bar className="my-[3px] h-[12px] w-[96px] rounded-xs" />
        <Bar className="my-[2px] h-[10px] w-[140px] max-w-full rounded-xs" />
      </DataTableCell>
      {shown.usedBy && (
        <DataTableCell>
          <Bar className="my-[3px] h-[12px] w-[56px] rounded-xs" />
        </DataTableCell>
      )}
      {shown.servers && (
        <DataTableCell>
          <Bar className="my-[3px] h-[12px] w-[112px] rounded-xs" />
        </DataTableCell>
      )}
      <DataTableCell />
    </DataTableRow>
  ))
}
