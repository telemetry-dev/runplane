import { Field as BaseField } from '@base-ui/react/field'
import { useQueryClient } from '@tanstack/react-query'
import { Send } from 'lucide-react'
import { useId, useRef, useState, type Ref } from 'react'
import { $api, type Schemas } from '~/api/client'
import { useFormProblems } from '~/api/form-problems'
import { limits } from '~/api/limits'
import { notificationsQuery } from '~/api/notifications'
import { Button } from '~/components/ui/button'
import { Checkbox, CheckboxGroup } from '~/components/ui/checkbox'
import { Dialog, DialogClose, DialogPopup } from '~/components/ui/dialog'
import { Field, Input } from '~/components/ui/field'
import { Fieldset } from '~/components/ui/fieldset'
import { Form } from '~/components/ui/form'
import { SegmentedControl } from '~/components/ui/segmented'
import { toastManager } from '~/components/ui/toast'
import { channelEvents, channelKinds, defaultEvents, type ChannelEvent, type ChannelKind } from './channel-info'
import { quietAction } from './parts'
import { useSendTest } from './send-test'

type Channel = Schemas['NotificationChannel']

type ChannelDialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** The channel to edit; without it the dialog adds one. */
  channel?: Channel
}

/** "Add channel" and "Edit channel": kind, name, webhook URL, and events. */
export function ChannelDialog({ open, onOpenChange, channel }: ChannelDialogProps) {
  const nameInput = useRef<HTMLInputElement>(null)
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogPopup title={channel ? 'Edit channel' : 'Add channel'} width={520} initialFocus={nameInput}>
        <ChannelForm channel={channel} nameInput={nameInput} onDone={() => onOpenChange(false)} />
      </DialogPopup>
    </Dialog>
  )
}

type ChannelFormProps = { channel?: Channel; nameInput: Ref<HTMLInputElement>; onDone: () => void }

// The popup unmounts when the dialog closes, so each opening starts from the channel again.
function ChannelForm({ channel, nameInput, onDone }: ChannelFormProps) {
  const client = useQueryClient()
  const formId = useId()
  const [kind, setKind] = useState<ChannelKind>(channel?.kind ?? 'slack')
  const [name, setName] = useState(channel?.name ?? '')
  const [url, setUrl] = useState('')
  const [events, setEvents] = useState<string[]>(channel?.events ?? defaultEvents)
  const kindInfo = channelKinds.find((item) => item.value === kind)!

  function saved(result: Channel) {
    onDone()
    toastManager.add({ title: channel ? `${result.name} saved` : `${result.name} added` })
    return client.invalidateQueries({ queryKey: notificationsQuery.queryKey })
  }
  const create = $api.useMutation('post', '/api/notifications', { onSuccess: saved })
  const update = $api.useMutation('patch', '/api/notifications/{id}', { onSuccess: saved })
  const save = channel ? update : create
  const sendTest = useSendTest()

  function submit() {
    const body = { kind, name: name.trim(), events: events as ChannelEvent[] }
    if (channel) update.mutate({ params: { path: { id: channel.id } }, body: { ...body, ...(url.trim() && { url: url.trim() }) } })
    else create.mutate({ body: { ...body, url: url.trim() } })
  }

  const problems = useFormProblems(save.error, { fields: formFields })
  const ready = name.trim() !== '' && (channel !== undefined || url.trim() !== '')

  return (
    <>
      <SegmentedControl aria-label="Kind" value={kind} onValueChange={setKind} options={channelKinds} />
      <Form id={formId} errors={problems.fields} onFormSubmit={submit} className="gap-l">
        <Field name="name" label="Name">
          <Input
            ref={nameInput}
            value={name}
            onValueChange={setName}
            placeholder={kindInfo.namePlaceholder}
            required
            maxLength={limits.CreateNotificationRequest.name.maxLength}
            autoComplete="off"
          />
        </Field>
        <BaseField.Root name="url" className="flex flex-col gap-xs">
          <BaseField.Label className="text-s font-medium text-neutral-10">Webhook URL</BaseField.Label>
          <Input
            type="url"
            value={url}
            onValueChange={setUrl}
            placeholder={channel ? `Leave empty to keep ${channel.url}` : undefined}
            required={!channel}
            autoComplete="off"
            spellCheck={false}
          />
          <BaseField.Description className="text-xs text-neutral-7">{kindInfo.urlHelp}</BaseField.Description>
          <BaseField.Error className="text-xs text-error" />
        </BaseField.Root>
        <Fieldset legend="Events">
          {/* Column first, as the design lists the job and server events on the left. */}
          <CheckboxGroup value={events} onValueChange={setEvents} className="grid grid-flow-col grid-cols-2 grid-rows-3 gap-x-l gap-y-s">
            {channelEvents.map((event) => (
              <Checkbox key={event.value} value={event.value} label={event.label} />
            ))}
          </CheckboxGroup>
        </Fieldset>
        {problems.alert && (
          <p role="alert" className="text-s text-error">
            {problems.alert}
          </p>
        )}
      </Form>
      <div className="flex flex-wrap items-center justify-between gap-xs">
        {/* A typed URL is tested as typed; an edit without one tests the saved channel. */}
        <button
          type="button"
          className={quietAction}
          disabled={sendTest.isPending || (!channel && !url.trim())}
          onClick={() =>
            sendTest.mutate(
              channel && !url.trim() ? { name: channel.name, id: channel.id } : { name: name.trim() || 'the channel', kind, url: url.trim() },
            )
          }
        >
          <Send aria-hidden />
          Send test
        </button>
        <div className="flex gap-xs">
          <DialogClose render={<Button />}>Cancel</DialogClose>
          <Button type="submit" form={formId} variant="primary" disabled={!ready} loading={save.isPending}>
            Save
          </Button>
        </div>
      </div>
    </>
  )
}

// The kind and the events have no Field, so their problems go to the alert.
const formFields = ['name', 'url']
