import { Field as BaseField } from '@base-ui/react/field'
import { RadioGroup } from '@base-ui/react/radio-group'
import { keepPreviousData, useQuery, useQueryClient } from '@tanstack/react-query'
import { useId, useRef, useState, type ReactNode, type RefObject } from 'react'
import { $api, type Schemas } from '~/api/client'
import { useFormProblems } from '~/api/form-problems'
import { imagesQuery } from '~/api/images'
import { limits } from '~/api/limits'
import { placementQuery, putPool } from '~/api/pools'
import { serversQuery } from '~/api/servers'
import { settingsQuery } from '~/api/settings'
import { LoadError } from '~/components/app/load-error'
import { serverStatusNames } from '~/components/app/status-dot'
import { serverSize, sortServers, statusFacts } from '~/components/pages/servers/server-info'
import { Button } from '~/components/ui/button'
import { Checkbox } from '~/components/ui/checkbox'
import { Dialog, DialogClose, DialogFooter, DialogPopup } from '~/components/ui/dialog'
import { Field, Input } from '~/components/ui/field'
import { Form } from '~/components/ui/form'
import { NumberInput } from '~/components/ui/number-field'
import { Radio } from '~/components/ui/radio-group'
import { SegmentedControl } from '~/components/ui/segmented'
import { Select, SelectItem, SelectLabel, SelectPopup, SelectTrigger } from '~/components/ui/select'
import { Spinner } from '~/components/ui/spinner'
import { toastManager } from '~/components/ui/toast'
import { cx } from '~/lib/cx'
import { draftFrom, formFields, poolRequest, poolUpdate, type Arch, type Draft } from './label-form'
import { formatTimeLimit, type Pool } from './runner-format'

type Server = Schemas['Server']

type LabelDialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** The pool to edit; without it the dialog makes a new one. */
  pool?: Pool
}

/** The "Runners · Edit label" design: label, size, arch, image, scaling, and servers of a pool. */
export function LabelDialog({ open, onOpenChange, pool }: LabelDialogProps) {
  const labelInput = useRef<HTMLInputElement>(null)
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogPopup title={pool ? 'Edit label' : 'New label'} width={560} initialFocus={labelInput}>
        <LabelFormLoader pool={pool} labelInput={labelInput} onSaved={() => onOpenChange(false)} />
      </DialogPopup>
    </Dialog>
  )
}

type FormProps = {
  pool: Pool | undefined
  labelInput: RefObject<HTMLInputElement | null>
  onSaved: () => void
}

// The form starts from the servers, images, and settings, so it waits for them.
function LabelFormLoader({ pool, labelInput, onSaved }: FormProps) {
  const servers = useQuery(serversQuery)
  const images = useQuery(imagesQuery)
  const settings = useQuery(settingsQuery)
  const failed = [servers, images, settings].find((query) => query.isError)

  if (failed) return <LoadError what="the form" error={failed.error} onRetry={() => void failed.refetch()} retrying={failed.isFetching} />
  if (!servers.data || !images.data || !settings.data) {
    return (
      <p role="status" className="flex items-center gap-s text-s text-neutral-7">
        <Spinner />
        Loading
      </p>
    )
  }
  return (
    <LabelForm
      pool={pool}
      labelInput={labelInput}
      servers={servers.data}
      images={images.data}
      settings={settings.data}
      onSaved={onSaved}
    />
  )
}

type LabelFormProps = FormProps & {
  servers: Server[]
  images: Schemas['Image'][]
  settings: Schemas['Settings']
}

function LabelForm({ pool, labelInput, servers, images, settings, onSaved }: LabelFormProps) {
  const client = useQueryClient()
  const defaultImage = images.find((image) => image.is_default)
  const primaryArch = servers.find((server) => server.kind === 'local')?.arch
  const [initial] = useState(() => draftFrom(pool, primaryArch === 'arm64' ? 'arm64' : 'x64', defaultImage?.id))
  const [draft, setDraft] = useState(initial)
  const [noServers, setNoServers] = useState(false)
  const serverArch = new Map(servers.map((server) => [server.id, server.arch]))

  const create = $api.useMutation('post', '/api/pools', { onSuccess: saved })
  const update = $api.useMutation('patch', '/api/pools/{id}', { onSuccess: saved })
  const save = pool ? update : create

  function saved(result: Pool) {
    putPool(client, result)
    toastManager.add({ title: `Saved ${result.label}` })
    onSaved()
  }

  function change(values: Partial<Draft>) {
    setDraft((current) => ({ ...current, ...values }))
  }

  function submit() {
    const body = poolRequest(draft, serverArch)
    if (!body) return
    if (!draft.allServers && body.server_ids.length === 0) {
      setNoServers(true)
      return
    }
    if (!pool) {
      create.mutate({ body })
      return
    }
    const changes = poolUpdate(pool, initial, body, serverArch)
    if (Object.keys(changes).length === 0) onSaved()
    else update.mutate({ params: { path: { id: pool.id } }, body: changes })
  }

  const problems = useFormProblems(save.error, { fields: formFields })
  const archServers = sortServers(servers.filter((server) => server.arch === draft.arch))
  const label = draft.label.trim()

  return (
    <Form errors={problems.fields} onFormSubmit={submit} className="gap-l">
      <Field name="label" label="Label" validate={validateLabel}>
        <Input
          ref={labelInput}
          value={draft.label}
          onValueChange={(value) => change({ label: value })}
          placeholder="runplane-4cpu"
          maxLength={poolLimits.label.maxLength}
          autoComplete="off"
          spellCheck={false}
          className="font-code text-xs leading-s tracking-m"
        />
        <Hint>
          In a workflow: <code className="font-code text-neutral-10">runs-on: {label}</code>
        </Hint>
        {pool && label !== pool.label && <Hint>Workflows that use {pool.label} stop getting runners.</Hint>}
      </Field>

      <div className="flex flex-col gap-xs">
        <div className="grid grid-cols-3 items-start gap-m">
          <Field name="vcpu" label={<Unit unit="vCPU">CPU</Unit>} validate={inRange(poolLimits.vcpu)}>
            <NumberInput
              value={draft.vcpu}
              onValueChange={(vcpu) => change({ vcpu })}
              min={poolLimits.vcpu.minimum}
              max={poolLimits.vcpu.maximum}
              suffix="vCPU"
              format={integer}
            />
          </Field>
          <Field name="memory_mb" label={<Unit unit="GB">Memory</Unit>} validate={inRange(memoryGb)}>
            <NumberInput
              value={draft.memoryGb}
              onValueChange={(memoryGb) => change({ memoryGb })}
              min={memoryGb.minimum}
              max={memoryGb.maximum}
              step={0.5}
              suffix="GB"
              format={{ useGrouping: false, maximumFractionDigits: 1 }}
            />
          </Field>
          <Field name="disk_gb" label={<Unit unit="GB">Disk</Unit>} validate={inRange(poolLimits.disk_gb)}>
            <NumberInput
              value={draft.diskGb}
              onValueChange={(diskGb) => change({ diskGb })}
              min={poolLimits.disk_gb.minimum}
              max={poolLimits.disk_gb.maximum}
              suffix="GB"
              format={integer}
            />
          </Field>
        </div>
        <FitHint draft={draft} />
      </div>

      <div className="flex items-start gap-m">
        <div className="flex shrink-0 flex-col gap-xs">
          <span aria-hidden className="text-s font-medium text-neutral-10">
            Arch
          </span>
          <SegmentedControl aria-label="Arch" value={draft.arch} onValueChange={(arch: Arch) => change({ arch })} options={archOptions} />
        </div>
        <ImageSelect
          images={images}
          pool={pool}
          value={draft.imageId}
          onValueChange={(imageId) => change({ imageId })}
          error={problems.fields.image_id}
        />
      </div>

      <div className="flex flex-col gap-xs">
        <div className="grid grid-cols-3 items-start gap-m">
          <Field name="min_idle" label="Warm runners" validate={inRange(poolLimits.min_idle)}>
            <NumberInput
              value={draft.minIdle}
              onValueChange={(minIdle) => change({ minIdle })}
              min={poolLimits.min_idle.minimum}
              max={poolLimits.min_idle.maximum}
              format={integer}
            />
          </Field>
          <Field name="max_runners" label="Max runners" validate={inRange(poolLimits.max_runners)}>
            <NumberInput
              value={draft.maxRunners}
              onValueChange={(maxRunners) => change({ maxRunners })}
              min={poolLimits.max_runners.minimum}
              max={poolLimits.max_runners.maximum}
              format={integer}
            />
          </Field>
          <TimeLimitSelect
            value={draft.timeLimit}
            defaultMinutes={settings.default_time_limit_minutes}
            onValueChange={(timeLimit) => change({ timeLimit })}
          />
        </div>
        <p className="text-xs text-neutral-7">Warm runners wait idle, so a job starts without the boot time. They count toward the max.</p>
      </div>

      <ServerChoice
        arch={draft.arch}
        servers={archServers}
        allServers={draft.allServers}
        selected={draft.serverIds}
        onChange={(values) => {
          setNoServers(false)
          change(values)
        }}
        error={noServers ? 'Select at least one server.' : problems.fields.server_ids}
      />

      {problems.alert && (
        <p role="alert" className="text-s text-error">
          {problems.alert}
        </p>
      )}

      <div className="pt-xs">
        <DialogFooter>
          <DialogClose render={<Button />}>Cancel</DialogClose>
          <Button type="submit" variant="primary" loading={save.isPending}>
            Save
          </Button>
        </DialogFooter>
      </div>
    </Form>
  )
}

/** "Fits on 1 of 5 x64 servers." under the size fields, from the placement rule of the scheduler. */
function FitHint({ draft }: { draft: Draft }) {
  const { vcpu, memoryGb, arch } = draft
  const sized = vcpu !== null && memoryGb !== null
  const serverIds = draft.allServers ? [] : draft.serverIds
  const query = {
    arch,
    vcpu: vcpu ?? 0,
    memory_mb: Math.round((memoryGb ?? 0) * 1024),
    server_ids: serverIds,
  }
  const placement = useQuery({ ...placementQuery(query), enabled: sized, placeholderData: keepPreviousData }).data
  if (!sized || !placement) return null
  const { placeable_servers: fits, online_servers: total } = placement
  if (total === 0) return <p className="text-xs text-neutral-10">No {arch} server is online.</p>
  if (fits === 0) return <p className="text-xs text-neutral-10">No {arch} server is large enough.</p>
  return (
    <p className="text-xs text-neutral-7">
      Fits on {fits} of {total} {arch} {total === 1 ? 'server' : 'servers'}.
    </p>
  )
}

type ImageSelectProps = {
  images: Schemas['Image'][]
  pool: Pool | undefined
  value: string
  onValueChange: (value: string) => void
  error: string | undefined
}

/**
 * A choice of the default image saves '' (a pool without an image), so the pool
 * follows a new default. The select itself needs a non-empty value, so it holds
 * the default image's ID.
 */
function ImageSelect({ images, pool, value, onValueChange, error }: ImageSelectProps) {
  const defaultId = images.find((image) => image.is_default)?.id ?? ''
  const selected = value || defaultId
  const items = images.map((image) => ({ value: image.id, label: image.name }))
  // An image that was removed still shows under its last name.
  if (pool && !items.some((item) => item.value === selected)) items.push({ value: selected, label: pool.image_name })
  return (
    <div className="flex min-w-0 grow flex-col gap-xs">
      <Select items={items} value={selected} onValueChange={(next) => onValueChange(!next || next === defaultId ? '' : next)}>
        <SelectLabel>Image</SelectLabel>
        <SelectTrigger />
        <SelectPopup>
          {items.map((item) => (
            <SelectItem key={item.value} value={item.value}>
              {item.label}
              {item.value === defaultId && <span className="text-neutral-7"> · default</span>}
            </SelectItem>
          ))}
        </SelectPopup>
      </Select>
      {error && (
        <p role="alert" className="text-xs text-error">
          {error}
        </p>
      )}
    </div>
  )
}

type TimeLimitSelectProps = {
  value: number
  defaultMinutes: number
  onValueChange: (value: number) => void
}

function TimeLimitSelect({ value, defaultMinutes, onValueChange }: TimeLimitSelectProps) {
  const minutes = timeLimits.includes(value) ? timeLimits : [...timeLimits, value].toSorted((a, b) => a - b)
  const items = minutes.map((limit) => ({
    value: limit,
    label:
      limit === 0 ? (
        <>
          Default <span className="text-neutral-7">· {formatTimeLimit(defaultMinutes)}</span>
        </>
      ) : (
        formatTimeLimit(limit)
      ),
  }))
  return (
    <div className="flex min-w-0 flex-col gap-xs">
      <Select items={items} value={value} onValueChange={(next) => onValueChange(next ?? 0)}>
        <SelectLabel>Time limit</SelectLabel>
        <SelectTrigger />
        <SelectPopup>
          {items.map((item) => (
            <SelectItem key={item.value} value={item.value}>
              {item.label}
            </SelectItem>
          ))}
        </SelectPopup>
      </Select>
    </div>
  )
}

type ServerChoiceProps = {
  arch: Arch
  /** The servers of the arch, in list order. */
  servers: Server[]
  allServers: boolean
  selected: string[]
  onChange: (values: Pick<Draft, 'allServers'> | Pick<Draft, 'serverIds'>) => void
  error: string | undefined
}

function ServerChoice({ arch, servers, allServers, selected, onChange, error }: ServerChoiceProps) {
  const labelId = useId()
  const errorId = useId()

  function toggle(id: string, checked: boolean) {
    onChange({ serverIds: checked ? [...selected, id] : selected.filter((item) => item !== id) })
  }

  return (
    <div role="group" aria-labelledby={labelId} className="flex flex-col gap-s">
      <span id={labelId} className="text-s font-medium text-neutral-10">
        Servers
      </span>
      <RadioGroup
        aria-labelledby={labelId}
        value={allServers ? 'all' : 'selected'}
        onValueChange={(value) => onChange({ allServers: value === 'all' })}
        className="flex flex-wrap gap-x-l gap-y-xs"
      >
        <Radio value="all" label={`All ${arch} servers`} />
        <Radio value="selected" label="Selected servers" />
      </RadioGroup>
      {!allServers && (
        <div
          aria-describedby={error ? errorId : undefined}
          // The edge and the dividers get their own pixels of padding, as borders would.
          className="relative overflow-hidden rounded-m p-px after:pointer-events-none after:absolute after:inset-0 after:rounded-[inherit] after:shadow-[inset_0_0_0_1px_var(--neutral-4)]"
        >
          <div className="max-h-[248px] overflow-y-auto">
            {servers.length === 0 && <p className="px-m py-s text-s text-neutral-7">There is no {arch} server.</p>}
            {servers.map((server) => (
              <label
                key={server.id}
                className="flex cursor-pointer items-center gap-s px-m py-xs text-s [&:not(:last-child)]:pb-[calc(var(--space-xs)+1px)] [&:not(:last-child)]:shadow-[inset_0_-1px_0_var(--neutral-4)]"
              >
                <Checkbox checked={selected.includes(server.id)} onCheckedChange={(checked) => toggle(server.id, checked)} />
                <span className={cx('min-w-0 grow truncate', statusFacts[server.status].muted ? 'text-neutral-7' : 'text-neutral-10')}>{server.name}</span>
                <span className="shrink-0 text-neutral-7">{serverDetail(server)}</span>
              </label>
            ))}
          </div>
        </div>
      )}
      {error && (
        <p id={errorId} role="alert" className="text-xs text-error">
          {error}
        </p>
      )}
    </div>
  )
}

/** Help text under a control in the dialog's XS size. */
function Hint({ children }: { children: ReactNode }) {
  return <BaseField.Description className="text-xs text-neutral-7">{children}</BaseField.Description>
}

/** A field label that names its unit for screen readers; the input shows the unit as a suffix. */
function Unit({ unit, children }: { unit: string; children: ReactNode }) {
  return (
    <>
      {children}
      <span className="sr-only"> in {unit}</span>
    </>
  )
}

function validateLabel(value: unknown): string | null {
  const label = String(value).trim()
  if (!label) return 'Enter a label.'
  return labelPattern.test(label) ? null : 'Use lowercase letters, digits, and dashes.'
}

function inRange({ minimum, maximum }: { minimum: number; maximum: number }) {
  return (value: unknown) => (typeof value === 'number' && value >= minimum && value <= maximum ? null : `Enter ${minimum} to ${maximum}.`)
}

/** "16 vCPU · 64 GB", after the state of a server that is not online: "Installing · 8 vCPU · 30 GB". */
function serverDetail(server: Server): string {
  const size = serverSize(server)
  if (server.status === 'online') return size
  return [serverStatusNames[server.status], size].filter(Boolean).join(' · ')
}

const timeLimits = [0, 30, 60, 120, 360, 720, 1440]

const poolLimits = limits.CreatePoolRequest

const labelPattern = new RegExp(poolLimits.label.pattern)

const archOptions = poolLimits.arch.enum.map((value) => ({ value, label: value }))

// The dialog shows memory in GB; the API takes megabytes.
const memoryGb = { minimum: poolLimits.memory_mb.minimum / 1024, maximum: poolLimits.memory_mb.maximum / 1024 }

const integer = { useGrouping: false, maximumFractionDigits: 0 }

