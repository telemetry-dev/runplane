import { Check } from 'lucide-react'
import type { ReactNode } from 'react'
import type { Schemas } from '~/api/client'
import {
  CellTitle,
  DataTable,
  DataTableCell,
  DataTableEmpty,
  DataTableHead,
  DataTableHeader,
  DataTableRow,
  useFittingColumns,
} from '~/components/app/data-table'
import { None } from '~/components/app/none'
import { SkeletonRows } from '~/components/app/skeleton-rows'
import { buildStateNames, StatusIcon } from '~/components/app/status-icon'
import { Spinner } from '~/components/ui/spinner'
import { formatBytes } from '~/lib/format'
import { buildOn, shortDigest, type Server } from './server-format'

type Image = Schemas['Image']
type ImageBuild = Schemas['ImageBuild']

type ImagesTableProps = {
  server: Server
  /** All images; undefined while they load. */
  images: readonly Image[] | undefined
  /** A LoadErrorRow in place of the loading rows, when the list did not load. */
  failed?: ReactNode
}

/** Each runner image and its build on this server. A narrow table drops Digest. */
export function ImagesTable({ server, images, failed }: ImagesTableProps) {
  const { ref, shown, columns } = useFittingColumns({ image: 'grow', digest: { width: 200, drop: 1 }, build: 220 })
  return (
    <div ref={ref}>
      <DataTable aria-label="Images" columns={columns} rowSpacing="m">
        <DataTableHeader>
          <DataTableHead>Image</DataTableHead>
          {shown.digest && <DataTableHead>Digest</DataTableHead>}
          <DataTableHead>On this server</DataTableHead>
        </DataTableHeader>
        {!images ? (
          failed ?? <SkeletonRows lines={2} label="Loading images" />
        ) : images.length === 0 ? (
          <DataTableEmpty>No images.</DataTableEmpty>
        ) : (
          images.map((image) => (
            <ImageRow key={image.id} image={image} build={buildOn(image, server.id)} vm={server.mode === 'vm'} digest={shown.digest} />
          ))
        )}
      </DataTable>
    </div>
  )
}

type ImageRowProps = { image: Image; build: ImageBuild | undefined; vm: boolean; digest: boolean }

function ImageRow({ image, build, vm, digest }: ImageRowProps) {
  return (
    <DataTableRow>
      <DataTableCell>
        <CellTitle
          title={
            <>
              {image.name}
              {image.is_default && <span className="font-regular text-neutral-7"> · default</span>}
            </>
          }
          detail={<span className="font-code tracking-m">{image.ref}</span>}
        />
      </DataTableCell>
      {digest && (
        <DataTableCell>
          {build?.digest ? (
            <span className="truncate font-code text-xs leading-s tracking-m">{shortDigest(build.digest)}</span>
          ) : (
            <None />
          )}
        </DataTableCell>
      )}
      <DataTableCell>
        <BuildState build={build} vm={vm} />
      </DataTableCell>
    </DataTableRow>
  )
}

function BuildState({ build, vm }: { build: ImageBuild | undefined; vm: boolean }) {
  if (!build) return <span className="text-neutral-6">Not on this server</span>
  if (build.status === 'ready') {
    return (
      <>
        <Check size={16} aria-hidden className="text-neutral-7" />
        <span className="truncate text-neutral-10">
          Ready
          {build.size_bytes > 0 && <span className="text-neutral-7"> · {formatBytes(build.size_bytes)}</span>}
        </span>
      </>
    )
  }
  if (build.status === 'failed') {
    return (
      <>
        <StatusIcon status="failed" className="self-start mt-[1px]" />
        <span className="flex min-w-0 flex-col" title={build.error || undefined}>
          <span className="text-neutral-10">Failed</span>
          {build.error && <span className="truncate text-xs text-neutral-7">{build.error}</span>}
        </span>
      </>
    )
  }
  return (
    <>
      <Spinner />
      <span className="truncate text-neutral-10">
        {buildStateNames[build.status]}
        {build.status === 'converting' && vm && <span className="text-neutral-7"> · to a VM disk</span>}
      </span>
    </>
  )
}
