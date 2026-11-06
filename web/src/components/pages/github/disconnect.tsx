import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api } from '~/api/client'
import { githubQuery } from '~/api/github'
import { ConfirmDialog } from '~/components/app/confirm-dialog'
import { Button } from '~/components/ui/button'

/** The last row of Settings · GitHub: stop the listeners, delete the scale sets, and remove the app from this instance. */
export function DisconnectGitHub({ appName }: { appName: string }) {
  const queryClient = useQueryClient()
  const [open, setOpen] = useState(false)

  return (
    <section className="flex flex-wrap items-start justify-between gap-x-xl gap-y-s pt-[calc(var(--space-l)+1px)] shadow-[inset_0_1px_0_var(--neutral-4)]">
      <div className="flex min-w-0 flex-1 basis-[320px] flex-col gap-xxs">
        <h2 className="text-s font-medium text-neutral-10">Disconnect GitHub</h2>
        <p className="text-s text-neutral-7">runplane stops taking jobs and deletes its runner scale sets on GitHub.</p>
      </div>
      <Button onClick={() => setOpen(true)}>Disconnect</Button>
      <ConfirmDialog
        open={open}
        onOpenChange={setOpen}
        title="Disconnect GitHub?"
        description={`runplane stops taking jobs, deletes its runner scale sets, and removes ${appName} from this instance. The app stays on GitHub until you delete it there.`}
        action="Disconnect GitHub"
        onConfirm={async () => {
          await api.DELETE('/api/github')
          await queryClient.invalidateQueries({ queryKey: githubQuery.queryKey })
        }}
      />
    </section>
  )
}
