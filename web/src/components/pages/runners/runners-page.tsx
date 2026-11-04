import { useSuspenseQuery } from '@tanstack/react-query'
import { useSearch } from '@tanstack/react-router'
import { Plus } from 'lucide-react'
import { useState } from 'react'
import { sessionQuery } from '~/api/queries'
import { SectionHeader } from '~/components/app/section-header'
import { Button } from '~/components/ui/button'
import { PageHeader } from '~/components/ui/page-header'
import { LabelDialog } from './label-dialog'
import { LabelsTable } from './labels-table'
import { LiveRunnersTable } from './live-runners-table'
import { poolsQuery, sandboxesQuery } from './queries'
import type { Pool } from './runner-format'

/** The Runners design: the labels (pools) with their live counts, and the runners that live now. */
export function RunnersPage() {
  const { data: pools } = useSuspenseQuery(poolsQuery)
  const { data: sandboxes } = useSuspenseQuery(sandboxesQuery())
  const admin = useSuspenseQuery(sessionQuery).data?.role === 'admin'
  const marked = useSearch({ from: '/_app/runners', select: (search) => search.label })
  // The pool stays while the dialog closes, so its content does not change during the animation.
  const [dialog, setDialog] = useState<{ open: boolean; pool?: Pool }>({ open: false })

  return (
    <>
      <PageHeader
        title="Runners"
        actions={
          admin && (
            <Button variant="primary" onClick={() => setDialog({ open: true })}>
              <Plus aria-hidden />
              New label
            </Button>
          )
        }
      />
      <section className="flex flex-col gap-s">
        <SectionHeader title="Labels" count={pools.length || undefined} />
        <LabelsTable pools={pools} admin={admin} marked={marked} onEdit={(pool) => setDialog({ open: true, pool })} />
      </section>
      {pools.length > 0 && (
        <section className="flex flex-col gap-s">
          <SectionHeader title="Live runners" count={sandboxes.length || undefined} />
          <LiveRunnersTable
            sandboxes={sandboxes}
            empty="No live runners. A runner starts when GitHub queues a job for one of these labels, or when a label has warm runners."
          />
        </section>
      )}
      <LabelDialog open={dialog.open} pool={dialog.pool} onOpenChange={(open) => setDialog((current) => ({ ...current, open }))} />
    </>
  )
}

/** The page while the pools and runners load. */
export function RunnersSkeleton() {
  return (
    <>
      <PageHeader title="Runners" />
      <section className="flex flex-col gap-s">
        <SectionHeader title="Labels" />
        <LabelsTable pools={undefined} admin={false} onEdit={() => {}} />
      </section>
      <section className="flex flex-col gap-s">
        <SectionHeader title="Live runners" />
        <LiveRunnersTable sandboxes={undefined} empty={null} />
      </section>
    </>
  )
}
