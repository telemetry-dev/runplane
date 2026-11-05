import { Copy, Ellipsis, ExternalLink, Pencil, Plus, RotateCcw, Terminal, Trash2 } from 'lucide-react'
import { useEffect, useMemo, useState, type ReactNode } from 'react'
import type { Schemas } from '~/api/client'
import { BarChart, type BarChartBar } from '~/components/app/bar-chart'
import { BranchRef } from '~/components/app/branch-ref'
import { Breadcrumbs, BreadcrumbLink, BreadcrumbPage } from '~/components/app/breadcrumbs'
import { ChartLegend } from '~/components/app/chart'
import { CodeBlock } from '~/components/app/code-block'
import { ConfirmDialog } from '~/components/app/confirm-dialog'
import { CopyButton } from '~/components/app/copy-button'
import {
  CellTitle,
  DataTable,
  DataTableCell,
  DataTableEmpty,
  DataTableHead,
  DataTableHeader,
  DataTableIconHead,
  DataTableRow,
  RowLink,
} from '~/components/app/data-table'
import { DoctorChecks } from '~/components/app/doctor-checks'
import { EmptyState } from '~/components/app/empty-state'
import { ErrorHint } from '~/components/app/error-hint'
import { FilterSelect } from '~/components/app/filter-select'
import { KeyValue, KeyValueList } from '~/components/app/key-value'
import { LineChart, type LinePoint } from '~/components/app/line-chart'
import { LoadError, NotFound } from '~/components/app/load-error'
import { LogViewer } from '~/components/app/log-viewer'
import { RunnerLabel } from '~/components/app/runner-label'
import { SectionButton, SectionHeader, SectionLink } from '~/components/app/section-header'
import { SkeletonCell, SkeletonRows } from '~/components/app/skeleton-rows'
import { Stat } from '~/components/app/stat'
import { StatusDot, type ServerStatus } from '~/components/app/status-dot'
import { RunnerStateIcon } from '~/components/app/runner-state-icon'
import { CheckIcon, StatusIcon, type CheckStatus, type JobStatus } from '~/components/app/status-icon'
import { UsageBar } from '~/components/app/usage-bar'
import { workflowDiff } from '~/components/pages/first-job/first-job'
import { clock } from '~/components/pages/jobs/job-format'
import { defaultRange, rangeOptions, statusOptions, type JobsRange, type JobsStatus } from '~/components/pages/jobs/jobs-search'
import { AlertDialog, AlertDialogClose, AlertDialogFooter, AlertDialogPopup, AlertDialogTrigger } from '~/components/ui/alert-dialog'
import { Avatar } from '~/components/ui/avatar'
import { Button, IconButton } from '~/components/ui/button'
import { Checkbox, CheckboxGroup } from '~/components/ui/checkbox'
import { Collapsible } from '~/components/ui/collapsible'
import { Combobox, ComboboxInput, ComboboxItem, ComboboxPopup } from '~/components/ui/combobox'
import { Dialog, DialogClose, DialogDescription, DialogFooter, DialogPopup, DialogTrigger } from '~/components/ui/dialog'
import { Drawer, DrawerClose, DrawerFooter, DrawerPopup, DrawerTrigger } from '~/components/ui/drawer'
import { Field, Input } from '~/components/ui/field'
import { Fieldset } from '~/components/ui/fieldset'
import { Form } from '~/components/ui/form'
import { SearchInput } from '~/components/ui/input'
import { Menu, MenuCheckboxItem, MenuGroup, MenuGroupLabel, MenuItem, MenuPopup, MenuRadioGroup, MenuRadioItem, MenuSeparator, MenuSubmenu, MenuSubmenuTrigger, MenuTrigger } from '~/components/ui/menu'
import { Meter } from '~/components/ui/meter'
import { NumberField, NumberInput } from '~/components/ui/number-field'
import { PageHeader } from '~/components/ui/page-header'
import { Popover, PopoverPopup, PopoverTrigger } from '~/components/ui/popover'
import { Progress } from '~/components/ui/progress'
import { Radio, RadioCard, RadioGroup } from '~/components/ui/radio-group'
import { ScrollArea } from '~/components/ui/scroll-area'
import { SegmentedControl } from '~/components/ui/segmented'
import { Select, SelectItem, SelectLabel, SelectPopup, SelectTrigger } from '~/components/ui/select'
import { Separator } from '~/components/ui/separator'
import { Spinner } from '~/components/ui/spinner'
import { Switch } from '~/components/ui/switch'
import { Tab, Tabs, TabsList, TabsPanel } from '~/components/ui/tabs'
import { toastManager } from '~/components/ui/toast'
import { Toggle, ToggleGroup } from '~/components/ui/toggle-group'
import { Tooltip } from '~/components/ui/tooltip'
import { DiscordIcon, GithubIcon, SlackIcon } from '~/components/ui/icons'
import { useThemeMode, type ThemeMode } from '~/lib/theme-mode'

/** Every shared component in its states, for design review. withGallery adds it to the routes of `pnpm dev`. */
export function ComponentGallery() {
  const [mode, setMode] = useThemeMode()
  return (
    <div className="min-h-dvh bg-neutral-1">
      <header className="sticky top-zero z-10 flex items-center justify-between gap-m bg-neutral-1 px-xxl py-l shadow-[inset_0_-1px_0_var(--neutral-4)]">
        <h1 className="type-page-title text-neutral-10">Components</h1>
        <SegmentedControl<ThemeMode>
          aria-label="Color mode"
          value={mode}
          onValueChange={setMode}
          options={[
            { value: 'system', label: 'System' },
            { value: 'light', label: 'Light' },
            { value: 'dark', label: 'Dark' },
          ]}
        />
      </header>
      <main className="mx-auto flex max-w-[1208px] flex-col gap-xxl px-xxl pt-xl pb-xxl">
        <Buttons />
        <TextFields />
        <Pickers />
        <Overlays />
        <Choices />
        <Feedback />
        <States />
        <Measures />
        <Statuses />
        <Tables />
        <PageParts />
        <Code />
        <Charts />
        <Logs />
      </main>
    </div>
  )
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="flex flex-col gap-l">
      <h2 className="type-section pb-xs text-neutral-10 shadow-[inset_0_-1px_0_var(--neutral-4)]">{title}</h2>
      {children}
    </section>
  )
}

function Specimen({ label, children, className }: { label: string; children: ReactNode; className?: string }) {
  return (
    <div className="flex flex-col gap-s">
      <span className="text-xs font-medium text-neutral-6">{label}</span>
      <div className={className ?? 'flex flex-wrap items-center gap-m'}>{children}</div>
    </div>
  )
}

/** A neutral-2 card, as the panels of the designs. */
function Card({ children, className }: { children: ReactNode; className?: string }) {
  return <div className={`flex flex-col gap-l rounded-m bg-neutral-2 p-l ${className ?? ''}`}>{children}</div>
}

function Buttons() {
  return (
    <Section title="Button">
      <Specimen label="Variants">
        <Button variant="primary">Re-run job</Button>
        <Button>View on GitHub</Button>
        <Button variant="outline">Outline</Button>
        <Button variant="ghost">Ghost</Button>
        <Button variant="danger">Remove server</Button>
      </Specimen>
      <Specimen label="With icons, as Job detail">
        <Button>
          <GithubIcon aria-hidden />
          View on GitHub
        </Button>
        <Button>
          <Terminal aria-hidden />
          Re-run with SSH
        </Button>
        <Button variant="primary">
          <RotateCcw aria-hidden />
          Re-run job
        </Button>
        <Button variant="primary">
          <Plus aria-hidden />
          Add server
        </Button>
      </Specimen>
      <Specimen label="Disabled and loading">
        <Button variant="primary" disabled>
          Done
        </Button>
        <Button disabled>Previous</Button>
        <Button variant="primary" loading>
          Saving
        </Button>
        <Button loading>Testing</Button>
      </Specimen>
      <Specimen label="Icon buttons">
        <IconButton label="Download log">
          <Copy aria-hidden />
        </IconButton>
        <IconButton label="More">
          <Ellipsis aria-hidden />
        </IconButton>
        <IconButton label="Disabled" disabled>
          <Trash2 aria-hidden />
        </IconButton>
        <Button>Run doctor</Button>
        <IconButton label="More actions" variant="secondary">
          <Ellipsis aria-hidden />
        </IconButton>
      </Specimen>
    </Section>
  )
}

function TextFields() {
  const [errors, setErrors] = useState<Record<string, string>>({})
  return (
    <Section title="Input, Field, Fieldset, Form">
      <div className="grid grid-cols-1 gap-xl md:grid-cols-2">
        <Card>
          <Specimen label="On a neutral-2 card" className="flex flex-col gap-m">
            <Field label="Organization" description="The GitHub organization that owns the app.">
              <Input defaultValue="kestrel-hq" />
            </Field>
            <Field label="Placeholder">
              <Input placeholder="ci.example.com" />
            </Field>
            <Field label="Read-only">
              <Input readOnly defaultValue="https://ci.kestrel.dev/webhooks/github" />
            </Field>
            <Field label="Disabled" disabled>
              <Input defaultValue="runplane-01" />
            </Field>
            <Field label="Setup code" description="The install script printed it. For a new code, run sudo runplane setup-code." wide>
              <Input defaultValue="7KQM-R4TX-W9HB" />
            </Field>
            <SearchInput placeholder="Search job, branch or commit" aria-label="Search jobs" />
          </Specimen>
        </Card>
        <Card>
          <Specimen label="Form with validation (submit to see errors)" className="flex flex-col gap-m">
            <Form
              errors={errors}
              onFormSubmit={(values) => setErrors(values.name === 'taken' ? { name: 'A server with this name exists.' } : {})}
            >
              <Fieldset legend="Connect over SSH">
                <Field name="name" label="Name" description='Type "taken" to see a server error.'>
                  <Input required placeholder="fsn1-ax52-c" />
                </Field>
                <Field name="host" label="Host" validate={(value) => (String(value).includes(' ') ? 'Hosts have no spaces.' : null)}>
                  <Input required placeholder="136.243.18.40" />
                </Field>
              </Fieldset>
              <div>
                <Button type="submit" variant="primary">
                  Connect
                </Button>
              </div>
            </Form>
          </Specimen>
        </Card>
      </div>
    </Section>
  )
}

const pools = [
  { value: 'runplane-2cpu', label: 'runplane-2cpu' },
  { value: 'runplane-4cpu', label: 'runplane-4cpu' },
  { value: 'runplane-8cpu', label: 'runplane-8cpu' },
  { value: 'runplane-4cpu-arm', label: 'runplane-4cpu-arm' },
]
const repositories = ['kestrel-hq/api', 'kestrel-hq/web', 'kestrel-hq/mobile', 'kestrel-hq/infra', 'ada-okafor/dotfiles']

function Pickers() {
  const [status, setStatus] = useState<JobsStatus | null>(null)
  const [repository, setRepository] = useState<string | null>(null)
  const [range, setRange] = useState<JobsRange>(defaultRange)
  return (
    <Section title="Select, Combobox, FilterSelect">
      <div className="grid grid-cols-1 gap-xl md:grid-cols-2">
        <Card>
          <Specimen label="Select" className="flex flex-col gap-xs">
            <Select items={pools} defaultValue="runplane-8cpu">
              <SelectLabel>Pool</SelectLabel>
              <SelectTrigger />
              <SelectPopup>
                {pools.map((pool) => (
                  <SelectItem key={pool.value} value={pool.value}>
                    {pool.label}
                  </SelectItem>
                ))}
              </SelectPopup>
            </Select>
          </Specimen>
          <Specimen label="Select with placeholder, disabled" className="flex flex-col gap-s">
            <Select items={pools}>
              <SelectTrigger placeholder="Choose a pool" aria-label="Pool" />
              <SelectPopup>
                {pools.map((pool) => (
                  <SelectItem key={pool.value} value={pool.value}>
                    {pool.label}
                  </SelectItem>
                ))}
              </SelectPopup>
            </Select>
            <Select items={pools} defaultValue="runplane-2cpu" disabled>
              <SelectTrigger aria-label="Pool" />
              <SelectPopup>
                <SelectItem value="runplane-2cpu">runplane-2cpu</SelectItem>
              </SelectPopup>
            </Select>
          </Specimen>
        </Card>
        <Card>
          <Specimen label="Combobox" className="flex flex-col gap-xs">
            <Field label="Repository">
              <Combobox items={repositories}>
                <ComboboxInput placeholder="Any repository" />
                <ComboboxPopup empty="No repositories match.">
                  {(name: string) => (
                    <ComboboxItem key={name} value={name}>
                      {name}
                    </ComboboxItem>
                  )}
                </ComboboxPopup>
              </Combobox>
            </Field>
          </Specimen>
        </Card>
      </div>
      <Specimen label="FilterSelect in the Jobs toolbar" className="flex flex-col">
        <div className="flex flex-wrap items-center gap-xs rounded-m bg-neutral-2 p-xs">
          <SearchInput placeholder="Search job, branch or commit" aria-label="Search jobs" className="min-w-[200px] grow basis-0" />
          <FilterSelect label="Status" allLabel="Any" value={status} onValueChange={setStatus} options={statusOptions} />
          <FilterSelect
            label="Repository"
            allLabel="All"
            value={repository}
            onValueChange={setRepository}
            options={repositories.map((name) => ({ value: name, label: name }))}
          />
          <FilterSelect label="Runner" allLabel="All" value={null} onValueChange={() => {}} options={pools} />
          <FilterSelect aria-label="Time range" value={range} onValueChange={(next) => next && setRange(next)} options={rangeOptions} />
        </div>
      </Specimen>
    </Section>
  )
}

function Overlays() {
  const [theme, setTheme] = useState('system')
  return (
    <Section title="Menu, Dialog, AlertDialog, Drawer, Popover, Tooltip">
      <Specimen label="Triggers">
        <Menu>
          <MenuTrigger render={<Button />}>Server actions</MenuTrigger>
          <MenuPopup>
            <MenuItem icon={<Pencil />}>Rename</MenuItem>
            <MenuItem icon={<RotateCcw />} detail="⌘R">
              Run doctor
            </MenuItem>
            <MenuSubmenu>
              <MenuSubmenuTrigger>Set status</MenuSubmenuTrigger>
              <MenuPopup side="right" align="start" sideOffset={0}>
                <MenuItem>Online</MenuItem>
                <MenuItem>Draining</MenuItem>
                <MenuItem disabled>Disabled</MenuItem>
              </MenuPopup>
            </MenuSubmenu>
            <MenuSeparator />
            <MenuGroup>
              <MenuGroupLabel>Theme</MenuGroupLabel>
              <MenuRadioGroup value={theme} onValueChange={setTheme}>
                <MenuRadioItem value="system">System</MenuRadioItem>
                <MenuRadioItem value="light">Light</MenuRadioItem>
                <MenuRadioItem value="dark">Dark</MenuRadioItem>
              </MenuRadioGroup>
            </MenuGroup>
            <MenuSeparator />
            <MenuCheckboxItem defaultChecked>Show idle sandboxes</MenuCheckboxItem>
            <MenuItem icon={<Trash2 />} danger>
              Remove server
            </MenuItem>
          </MenuPopup>
        </Menu>

        <Dialog>
          <DialogTrigger render={<Button variant="primary" />}>
            <Plus aria-hidden />
            Add server
          </DialogTrigger>
          <DialogPopup title="Add server" width={520}>
            <Tabs defaultValue="script">
              <TabsList aria-label="Connection">
                <Tab value="script">Install script</Tab>
                <Tab value="ssh">Connect over SSH</Tab>
              </TabsList>
              <TabsPanel value="script">
                <DialogDescription>
                  Run this on a fresh Ubuntu 22.04+ or Debian 12 server. It installs Docker and the runplane agent, then connects to
                  ci.kestrel.dev.
                </DialogDescription>
                <CodeBlock copyable code={['curl -fsSL https://ci.kestrel.dev/install.sh | sudo sh -s -- \\', '  --token rp_srv_8Fq2nL7xVb3kR9tW']} />
                <p className="text-xs text-neutral-7">The token works once and expires in 1 hour.</p>
              </TabsPanel>
              <TabsPanel value="ssh">
                <Field label="Host">
                  <Input placeholder="136.243.18.40" />
                </Field>
              </TabsPanel>
            </Tabs>
            <div className="flex items-center gap-s py-s shadow-[inset_0_1px_0_var(--neutral-4),inset_0_-1px_0_var(--neutral-4)]">
              <Spinner />
              <span className="text-s font-medium text-neutral-10">Waiting for the server to connect</span>
            </div>
            <DialogFooter>
              <DialogClose render={<Button />}>Cancel</DialogClose>
              <Button variant="primary" disabled>
                Done
              </Button>
            </DialogFooter>
          </DialogPopup>
        </Dialog>

        <AlertDialog>
          <AlertDialogTrigger render={<Button variant="danger" />}>Remove server</AlertDialogTrigger>
          <AlertDialogPopup title="Remove fsn1-ax52-b?" description="Its 2 running jobs are cancelled, and runplane uninstalls the agent.">
            <AlertDialogFooter>
              <AlertDialogClose render={<Button />}>Cancel</AlertDialogClose>
              <AlertDialogClose render={<Button variant="danger" />}>Remove server</AlertDialogClose>
            </AlertDialogFooter>
          </AlertDialogPopup>
        </AlertDialog>

        <Drawer>
          <DrawerTrigger render={<Button />}>Bottom drawer</DrawerTrigger>
          <DrawerPopup title="runplane-8cpu" description="8 vCPU · 32 GB · x64">
            <KeyValueList className="gap-x-xl">
              <KeyValue label="Idle">2</KeyValue>
              <KeyValue label="Busy">5</KeyValue>
              <KeyValue label="Queued">1</KeyValue>
            </KeyValueList>
            <DrawerFooter>
              <DrawerClose render={<Button />}>Close</DrawerClose>
            </DrawerFooter>
          </DrawerPopup>
        </Drawer>

        <Drawer swipeDirection="right">
          <DrawerTrigger render={<Button />}>Side drawer</DrawerTrigger>
          <DrawerPopup title="Sandbox 01JC8…" description="Busy · fsn1-ax52-a">
            <p className="text-s text-neutral-8">Swipe right or press Escape to close.</p>
          </DrawerPopup>
        </Drawer>

        <Popover>
          <PopoverTrigger render={<Button />}>Why is it queued?</PopoverTrigger>
          <PopoverPopup title="Waiting for capacity" description="No server has 16 vCPU free. The job starts when a running job ends." />
        </Popover>

        <Tooltip content="Copies the runs-on label">
          <Button>Hover for a tooltip</Button>
        </Tooltip>
      </Specimen>
    </Section>
  )
}

function Choices() {
  const [range, setRange] = useState<'24h' | '7d' | '30d'>('24h')
  return (
    <Section title="Tabs, SegmentedControl, ToggleGroup, Switch, Radio, Checkbox">
      <div className="grid grid-cols-1 gap-xl md:grid-cols-2">
        <div className="flex flex-col gap-xl">
          <Specimen label="Tabs with the moving indicator" className="flex flex-col">
            <Tabs defaultValue="github">
              <TabsList aria-label="Settings sections">
                <Tab value="general">General</Tab>
                <Tab value="github">GitHub</Tab>
                <Tab value="runners">Runners</Tab>
                <Tab value="notifications">Notifications</Tab>
                <Tab value="members" disabled>
                  Members
                </Tab>
              </TabsList>
              <TabsPanel value="general" className="text-s text-neutral-8">
                Instance name, URL, and backups.
              </TabsPanel>
              <TabsPanel value="github" className="text-s text-neutral-8">
                App, installations, and job access.
              </TabsPanel>
              <TabsPanel value="runners" className="text-s text-neutral-8">
                Host reserve and runner versions.
              </TabsPanel>
              <TabsPanel value="notifications" className="text-s text-neutral-8">
                Channels.
              </TabsPanel>
            </Tabs>
          </Specimen>
          <Specimen label="SegmentedControl (Overview range)">
            <SegmentedControl
              aria-label="Range"
              value={range}
              onValueChange={setRange}
              options={[
                { value: '24h', label: '24h' },
                { value: '7d', label: '7d' },
                { value: '30d', label: '30d' },
              ]}
            />
          </Specimen>
          <Specimen label="ToggleGroup and Toggle">
            <ToggleGroup multiple defaultValue={['wrap']} aria-label="Log view">
              <Toggle value="wrap">Wrap</Toggle>
              <Toggle value="timestamps">Timestamps</Toggle>
              <Toggle value="raw" disabled>
                Raw
              </Toggle>
            </ToggleGroup>
            <Toggle aria-label="Follow">Follow</Toggle>
          </Specimen>
        </div>
        <div className="flex flex-col gap-xl">
          <Specimen label="Switch">
            <Switch label="Off" />
            <Switch label="On" defaultChecked />
            <Switch label="Disabled" disabled />
            <Switch label="Disabled on" disabled defaultChecked />
          </Specimen>
          <Specimen label="Switch rows, as Job access" className="flex flex-col">
            <div className="flex flex-col [&>*]:py-m [&>*:not(:last-child)]:shadow-[inset_0_-1px_0_var(--neutral-4)]">
              <Switch label="Run jobs from public repositories" description="Anyone who opens a pull request could run code on your servers." />
              <Switch label="Remove runners after each job" description="Each job gets a clean container, and its runner is removed when the job ends." defaultChecked />
              <Switch label="Allow fork pull requests" description="Jobs from forks run on your servers." error="Only admins can change this." />
            </div>
          </Specimen>
        </div>
      </div>
      <div className="grid grid-cols-1 gap-xl md:grid-cols-2">
        <Card>
          <Specimen label="Radio cards, as Install on" className="flex flex-col">
            <RadioGroup defaultValue="organization" direction="row" aria-label="Install on">
              <RadioCard value="organization" title="Organization" description="Every repository in the organization can use your runners." />
              <RadioCard value="user" title="Personal account" description="Only repositories you own can use your runners." />
            </RadioGroup>
          </Specimen>
        </Card>
        <div className="flex flex-col gap-xl">
          <Specimen label="RadioGroup">
            <RadioGroup defaultValue="https" aria-label="Protocol">
              <Radio value="https" label="HTTPS with Let's Encrypt" />
              <Radio value="http" label="HTTP only" />
              <Radio value="proxy" label="Behind a proxy" disabled />
            </RadioGroup>
          </Specimen>
          <Specimen label="Checkbox and CheckboxGroup">
            <Checkbox label="Unchecked" />
            <Checkbox label="Checked" defaultChecked />
            <Checkbox label="Indeterminate" indeterminate />
            <Checkbox label="Disabled" disabled defaultChecked />
          </Specimen>
          <ParentCheckboxes />
        </div>
      </div>
    </Section>
  )
}

const events = ['workflow_job', 'installation', 'push']

function ParentCheckboxes() {
  const [value, setValue] = useState(['workflow_job'])
  return (
    <CheckboxGroup value={value} onValueChange={setValue} allValues={events} aria-label="Webhook events">
      <Checkbox parent label="All events" />
      {events.map((event) => (
        <Checkbox key={event} value={event} label={event} className="ml-l" />
      ))}
    </CheckboxGroup>
  )
}

function Feedback() {
  return (
    <Section title="Toast">
      <Specimen label="Toasts stack at the bottom right">
        <Button onClick={() => toastManager.add({ title: 'Server removed', description: 'fsn1-ax52-b no longer takes jobs.' })}>Plain</Button>
        <Button
          onClick={() =>
            toastManager.add({
              title: 'Job cancelled',
              description: 'e2e · webkit stopped after 2m 14s.',
              actionProps: { children: 'Undo', onClick: () => toastManager.add({ title: 'Job queued again' }) },
            })
          }
        >
          With action
        </Button>
        <Button
          onClick={() =>
            void toastManager.promise(new Promise((resolve) => setTimeout(resolve, 1500)), {
              loading: { title: 'Pulling runner image', type: 'loading' },
              success: { title: 'Image ready', description: 'ghcr.io/runplane/ubuntu-24.04' },
              error: 'Pull failed',
            })
          }
        >
          Promise
        </Button>
      </Specimen>
    </Section>
  )
}

const demoChecks: Schemas['Check'][] = [
  { id: 'kernel', name: 'Kernel', status: 'ok', detail: 'Linux 6.8.0-49-generic', fix: '', command: '' },
  {
    id: 'ip_forward',
    name: 'IP forwarding',
    status: 'fail',
    detail: 'net.ipv4.ip_forward is 0, so sandboxes cannot reach the internet.',
    fix: 'Turn on IP forwarding and keep it on after a restart.',
    command: "echo 'net.ipv4.ip_forward=1' | sudo tee /etc/sysctl.d/99-runplane.conf && sudo sysctl --system",
  },
  { id: 'time', name: 'Clock', status: 'warn', detail: 'The clock is 4 seconds ahead of pool.ntp.org.', fix: 'Turn on time sync with timedatectl.', command: '' },
  { id: 'disk', name: 'Free disk', status: 'ok', detail: '1.7 TB free of 1.9 TB', fix: '', command: '' },
]

function States() {
  const [dialog, setDialog] = useState<'remove' | 'drain'>()
  const close = (open: boolean) => !open && setDialog(undefined)
  return (
    <Section title="ConfirmDialog, LoadError, NotFound, SkeletonRows, DoctorChecks, ErrorHint">
      <Specimen label="ConfirmDialog: Remove fails, Drain succeeds after a second">
        <Button variant="danger" onClick={() => setDialog('remove')}>
          Remove server
        </Button>
        <Button onClick={() => setDialog('drain')}>Drain</Button>
        <ConfirmDialog
          open={dialog === 'remove'}
          onOpenChange={close}
          title="Remove fsn1-ax52-b?"
          description="Its 2 running jobs are cancelled, and runplane disconnects the agent."
          action="Remove server"
          onConfirm={() => Promise.reject(new Error('runplane runs on this server, so it cannot be removed.'))}
        />
        <ConfirmDialog
          open={dialog === 'drain'}
          onOpenChange={close}
          title="Drain fsn1-ax52-b?"
          description="New jobs go to other servers. Running jobs finish."
          action="Drain server"
          variant="primary"
          onConfirm={() => new Promise((resolve) => setTimeout(resolve, 1000))}
        />
      </Specimen>
      <Specimen label="LoadError, in a section and in place of a page body" className="flex flex-col gap-l">
        <LoadError what="the backups" error={new Error('Bad Gateway')} onRetry={() => {}} />
        <LoadError heading={2} what="the overview" error={new Error('Bad Gateway')} onRetry={() => {}} retrying />
      </Specimen>
      <Specimen label="NotFound" className="flex flex-col">
        <NotFound title="Job not found" to="/jobs" link="All jobs">
          No job has this ID. Jobs older than the job retention are deleted.
        </NotFound>
      </Specimen>
      <Specimen label="SkeletonRows: a bar across the table, and cells" className="grid grid-cols-1 gap-xl md:grid-cols-2">
        <DataTable aria-label="Labels" columns={['grow', 150, 48]} rowSpacing="m">
          <DataTableHeader>
            <DataTableHead>Label</DataTableHead>
            <DataTableHead>Size</DataTableHead>
            <DataTableHead align="end">Busy</DataTableHead>
          </DataTableHeader>
          <SkeletonRows lines={2} label="Loading labels" />
        </DataTable>
        <DataTable aria-label="Members" columns={[28, 'grow', 120, 24]}>
          <DataTableHeader>
            <DataTableHead />
            <DataTableHead>Name</DataTableHead>
            <DataTableHead>Role</DataTableHead>
            <DataTableHead />
          </DataTableHeader>
          <SkeletonRows label="Loading members" count={2}>
            <SkeletonCell shape="large-avatar" />
            <SkeletonCell lines={[96, 120]} />
            <SkeletonCell shape="select" />
            <SkeletonCell />
          </SkeletonRows>
        </DataTable>
      </Specimen>
      <Specimen label="DoctorChecks" className="flex flex-col">
        <DoctorChecks checks={demoChecks} />
      </Specimen>
      <Specimen label="ErrorHint, on an icon and on text">
        <ErrorHint error="pull ghcr.io/kestrel-hq/android-sdk:34: unauthorized">
          <StatusIcon status="failed" />
        </ErrorHint>
        <span className="text-s text-neutral-7">
          v1.4.0 ·{' '}
          <ErrorHint error="The new agent did not connect within 2 minutes." className="inline underline decoration-dotted underline-offset-2">
            Update failed
          </ErrorHint>
        </span>
      </Specimen>
    </Section>
  )
}

function Measures() {
  return (
    <Section title="Progress, Meter, NumberField, Separator, ScrollArea, Avatar, Collapsible">
      <div className="grid grid-cols-1 gap-xl md:grid-cols-3">
        <Specimen label="Progress" className="flex flex-col gap-l">
          <Progress label="Pulling image" value={64} />
          <Progress label="Waiting for a runner" value={null} />
        </Specimen>
        <Specimen label="Meter" className="flex flex-col gap-l">
          <Meter label="Disk" value={71} />
          <Meter aria-label="Memory" value={22} />
        </Specimen>
        <Card>
          <Specimen label="NumberField" className="flex flex-col gap-m">
            <NumberField label="Concurrent jobs" defaultValue={4} min={1} max={64} />
            <NumberField aria-label="Disabled" defaultValue={2} disabled />
            <Field label="Memory">
              <NumberInput defaultValue={16} min={1} suffix="GB" />
            </Field>
          </Specimen>
        </Card>
      </div>
      <div className="grid grid-cols-1 gap-xl md:grid-cols-3">
        <Specimen label="Separator" className="flex flex-col gap-s">
          <span className="text-s text-neutral-8">Above</span>
          <Separator />
          <div className="flex items-center gap-s text-s text-neutral-8">
            Left <Separator orientation="vertical" /> Right
          </div>
        </Specimen>
        <Specimen label="ScrollArea" className="flex">
          <ScrollArea className="h-[160px] w-full rounded-s bg-neutral-2" contentClassName="flex flex-col gap-xs p-m text-s text-neutral-8">
            {Array.from({ length: 20 }, (_, index) => (
              <span key={index}>runplane-{index + 1}</span>
            ))}
          </ScrollArea>
        </Specimen>
        <Specimen label="Avatar" className="flex items-center gap-m">
          <Avatar name="Ada Okafor" />
          <Avatar name="kestrel-hq" size="l" />
          <Avatar name="ada-okafor" size="l" src="/missing.png" />
        </Specimen>
      </div>
      <Specimen label="Collapsible" className="flex max-w-[420px] flex-col">
        <Collapsible title="Advanced network settings">
          Sandboxes reach the host only through the cache and registry mirror ports. Outbound traffic goes through NAT.
        </Collapsible>
      </Specimen>
    </Section>
  )
}

const jobStatuses: JobStatus[] = ['running', 'queued', 'succeeded', 'failed', 'cancelled', 'skipped']
const serverStatuses: ServerStatus[] = ['online', 'installing', 'unreachable', 'draining', 'disabled']
const checkStatuses: CheckStatus[] = ['ok', 'warn', 'fail']

function Statuses() {
  return (
    <Section title="StatusIcon, StatusDot, Spinner">
      {[14, 16, 22].map((size) => (
        <Specimen key={size} label={`StatusIcon ${size}px`}>
          {jobStatuses.map((status) => (
            <span key={status} className="flex items-center gap-xs text-s text-neutral-8">
              <StatusIcon status={status} size={size} />
              {status}
            </span>
          ))}
        </Specimen>
      ))}
      <Specimen label="StatusDot">
        {serverStatuses.map((status) => (
          <span key={status} className="flex items-center gap-xs text-s text-neutral-8">
            <StatusDot status={status} />
            {status}
          </span>
        ))}
      </Specimen>
      {[14, 16].map((size) => (
        <Specimen key={size} label={`CheckIcon ${size}px`}>
          {checkStatuses.map((status) => (
            <span key={status} className="flex items-center gap-xs text-s text-neutral-8">
              <CheckIcon status={status} size={size} />
              {status}
            </span>
          ))}
          <span className="flex items-center gap-xs text-s text-neutral-8">
            <CheckIcon status="ok" size={size} quiet />
            ok, quiet
          </span>
        </Specimen>
      ))}
      <Specimen label="RunnerStateIcon">
        {(['starting', 'idle', 'busy', 'stopping'] as const).map((state) => (
          <span key={state} className="flex items-center gap-xs text-s text-neutral-8">
            <RunnerStateIcon state={state} />
            {state}
          </span>
        ))}
      </Specimen>
      <Specimen label="Brand icons">
        <GithubIcon size={16} aria-label="GitHub" />
        <SlackIcon size={16} aria-label="Slack" />
        <DiscordIcon size={16} aria-label="Discord" />
      </Specimen>
    </Section>
  )
}

type DemoJob = {
  status: JobStatus
  name: string
  workflow: string
  branch: string
  sha: string
  runner: string
  server: string | null
  queued: string
  duration: string | null
  started: string
}

const demoJobs: DemoJob[] = [
  { status: 'running', name: 'test (node 22)', workflow: 'kestrel-hq/api · ci.yml · #4182', branch: 'feat/billing-v2', sha: '9f3c2e1aa', runner: 'runplane-8cpu', server: 'fsn1-ax52-a', queued: '2s', duration: '1m 42s', started: '2m ago' },
  { status: 'queued', name: 'integration (postgres 16)', workflow: 'kestrel-hq/api · ci.yml · #4182', branch: 'feat/billing-v2', sha: '9f3c2e1', runner: 'runplane-16cpu', server: null, queued: '41s', duration: null, started: '1m ago' },
  { status: 'failed', name: 'e2e · webkit', workflow: 'kestrel-hq/web · e2e.yml · #2290', branch: 'fix/checkout-redirect', sha: 'c4e8f12', runner: 'runplane-8cpu', server: 'fsn1-ax52-a', queued: '4s', duration: '6m 48s', started: '11m ago' },
  { status: 'succeeded', name: 'typecheck', workflow: 'kestrel-hq/web · ci.yml · #5520', branch: 'fix/checkout-redirect', sha: 'c4e8f12', runner: 'runplane-4cpu', server: 'fsn1-ax102-a', queued: '1s', duration: '1m 05s', started: '11m ago' },
  { status: 'cancelled', name: 'deploy-staging', workflow: 'kestrel-hq/api · deploy.yml · #917', branch: 'main', sha: '1d0e7aa', runner: 'runplane-2cpu', server: 'fsn1-ax52-b', queued: '1s', duration: '14s', started: '14m ago' },
  { status: 'succeeded', name: 'lint', workflow: 'kestrel-hq/mobile · ci.yml · #1204', branch: 'renovate/expo-52', sha: '77b2a9d', runner: 'runplane-4cpu-arm', server: 'hel1-cax41', queued: '3s', duration: '38s', started: '16m ago' },
]

const demoServers: Array<{ status: ServerStatus; name: string; detail: string; size: string; disk: string; cpu: number | null; memory: number | null; note?: [string, string]; jobs: string }> = [
  { status: 'online', name: 'fsn1-ax102-a', detail: '136.243.18.40 · Hetzner FSN1 · Ubuntu 24.04', size: '32 vCPU · 128 GB', disk: 'x64 · 1.9 TB NVMe', cpu: 52, memory: 38, jobs: '3 jobs' },
  { status: 'online', name: 'fsn1-ax52-a', detail: '136.243.18.77 · Hetzner FSN1 · Ubuntu 24.04', size: '16 vCPU · 64 GB', disk: 'x64 · 1 TB NVMe', cpu: 88, memory: 61, jobs: '2 jobs' },
  { status: 'installing', name: 'gra-b2-30', detail: '51.68.42.110 · OVH GRA · Debian 12', size: '8 vCPU · 30 GB', disk: 'x64 · 200 GB SSD', cpu: null, memory: null, note: ['Installing runner agent', '· pulling runner image'], jobs: '' },
  { status: 'unreachable', name: 'ash-dedi-01', detail: '5.161.80.3 · Hetzner ASH · Ubuntu 22.04', size: '8 vCPU · 32 GB', disk: 'x64 · 512 GB NVMe', cpu: null, memory: null, note: ['Unreachable', '· last heartbeat 2h ago'], jobs: '' },
]

function Tables() {
  return (
    <Section title="DataTable">
      <div className="flex flex-col gap-s">
        <SectionHeader title="Running" count={7} />
        <DataTable aria-label="Running jobs" columns={[16, 'grow', 200, 150, 120, 96, 56]}>
          <DataTableHeader>
            <DataTableHead />
            <DataTableHead>Job</DataTableHead>
            <DataTableHead>Branch</DataTableHead>
            <DataTableHead>Runner</DataTableHead>
            <DataTableHead>Server</DataTableHead>
            <DataTableHead>Progress</DataTableHead>
            <DataTableHead align="end">Elapsed</DataTableHead>
          </DataTableHeader>
          {[53, 69, 29, null].map((progress, index) => (
            <DataTableRow key={index}>
              <DataTableCell>
                <StatusIcon status="running" />
              </DataTableCell>
              <DataTableCell>
                <RowLink to="." hash="tables">
                  <CellTitle title={['test (node 22)', 'e2e · chromium', 'build-image', 'lint'][index]} detail="kestrel-hq/api · ci.yml" />
                </RowLink>
              </DataTableCell>
              <DataTableCell>
                <BranchRef branch={['feat/billing-v2', 'fix/checkout-redirect', 'main', 'renovate/expo-52'][index]!} />
              </DataTableCell>
              <DataTableCell>
                <RunnerLabel>runplane-8cpu</RunnerLabel>
              </DataTableCell>
              <DataTableCell>fsn1-ax52-a</DataTableCell>
              <DataTableCell>
                <UsageBar value={progress} label="Progress" className="w-full" />
              </DataTableCell>
              <DataTableCell align="end">{['1m 42s', '3m 05s', '48s', '12s'][index]}</DataTableCell>
            </DataTableRow>
          ))}
        </DataTable>
      </div>

      <DataTable aria-label="Jobs" columns={[16, 'grow', 200, 150, 120, 64, 72, 72]}>
        <DataTableHeader>
          <DataTableHead />
          <DataTableHead>Job</DataTableHead>
          <DataTableHead>Branch</DataTableHead>
          <DataTableHead>Runner</DataTableHead>
          <DataTableHead>Server</DataTableHead>
          <DataTableHead align="end">Queued</DataTableHead>
          <DataTableHead align="end">Duration</DataTableHead>
          <DataTableHead align="end">Started</DataTableHead>
        </DataTableHeader>
        {demoJobs.map((job, index) => (
          <DataTableRow key={index} selected={index === 2}>
            <DataTableCell>
              <StatusIcon status={job.status} />
            </DataTableCell>
            <DataTableCell>
              <RowLink to="." hash="tables">
                <CellTitle title={job.name} detail={job.workflow} muted={job.status === 'cancelled'} />
              </RowLink>
            </DataTableCell>
            <DataTableCell>
              <BranchRef branch={job.branch} sha={job.sha} />
            </DataTableCell>
            <DataTableCell>
              <RunnerLabel>{job.runner}</RunnerLabel>
            </DataTableCell>
            <DataTableCell className={job.server ? undefined : 'text-neutral-6'}>{job.server ?? '—'}</DataTableCell>
            <DataTableCell align="end" className={job.status === 'queued' ? 'text-neutral-10' : undefined}>
              {job.queued}
            </DataTableCell>
            <DataTableCell align="end" className={job.duration ? (job.status === 'running' ? 'text-neutral-10' : undefined) : 'text-neutral-6'}>
              {job.duration ?? '—'}
            </DataTableCell>
            <DataTableCell align="end" className="text-neutral-7">
              {job.started}
            </DataTableCell>
          </DataTableRow>
        ))}
      </DataTable>
      <div className="flex items-center justify-between">
        <span className="text-s text-neutral-7">1–50 of 1,284</span>
        <div className="flex gap-xs">
          <Button disabled>Previous</Button>
          <Button>Next</Button>
        </div>
      </div>

      <DataTable aria-label="Servers" columns={[16, 'grow', 170, 140, 140, 64, 24]} rowSpacing="m">
        <DataTableHeader>
          <DataTableIconHead label="Status" />
          <DataTableHead>Server</DataTableHead>
          <DataTableHead>Resources</DataTableHead>
          <DataTableHead>CPU</DataTableHead>
          <DataTableHead>Memory</DataTableHead>
          <DataTableHead align="end">Running</DataTableHead>
          <DataTableHead />
        </DataTableHeader>
        {demoServers.map((server) => (
          <DataTableRow key={server.name}>
            <DataTableCell>
              <StatusDot status={server.status} />
            </DataTableCell>
            <DataTableCell>
              <CellTitle title={server.name} detail={server.detail} muted={server.status === 'unreachable'} />
            </DataTableCell>
            <DataTableCell>
              <CellTitle title={<span className="font-regular text-neutral-8">{server.size}</span>} detail={server.disk} />
            </DataTableCell>
            {server.note ? (
              <DataTableCell className="col-span-3">
                <span className="font-medium text-neutral-10">{server.note[0]}</span>
                <span className="text-neutral-7">{server.note[1]}</span>
              </DataTableCell>
            ) : (
              <>
                <DataTableCell>
                  <UsageBar value={server.cpu} label="CPU" showValue className="w-full" />
                </DataTableCell>
                <DataTableCell>
                  <UsageBar value={server.memory} label="Memory" showValue className="w-full" />
                </DataTableCell>
                <DataTableCell align="end">{server.jobs}</DataTableCell>
              </>
            )}
            <DataTableCell align="end" className="relative">
              <Menu>
                <MenuTrigger render={<IconButton label={`${server.name} actions`} className="-m-xs" />}>
                  <Ellipsis aria-hidden />
                </MenuTrigger>
                <MenuPopup align="end">
                  <MenuItem>Run doctor</MenuItem>
                  <MenuItem danger>Remove</MenuItem>
                </MenuPopup>
              </Menu>
            </DataTableCell>
          </DataTableRow>
        ))}
      </DataTable>

      <DataTable aria-label="Labels you can use" columns={['grow', 160, 64]} rowSpacing="xs">
        <DataTableHeader>
          <DataTableHead>Label</DataTableHead>
          <DataTableHead>Size</DataTableHead>
          <DataTableHead align="end">Arch</DataTableHead>
        </DataTableHeader>
        <DataTableEmpty>No pools yet.</DataTableEmpty>
      </DataTable>
    </Section>
  )
}

function PageParts() {
  return (
    <Section title="PageHeader, Breadcrumbs, SectionHeader, KeyValue, Stat, EmptyState">
      <div className="flex flex-col gap-s">
        <Breadcrumbs>
          <BreadcrumbLink to=".">Jobs</BreadcrumbLink>
          <BreadcrumbLink to=".">kestrel-hq/web</BreadcrumbLink>
          <BreadcrumbPage>e2e.yml #2290</BreadcrumbPage>
        </Breadcrumbs>
        <PageHeader
          icon={<StatusIcon status="failed" size={22} />}
          title="e2e · webkit"
          actions={
            <>
              <Button>
                <GithubIcon aria-hidden />
                View on GitHub
              </Button>
              <Button>
                <Terminal aria-hidden />
                Re-run with SSH
              </Button>
              <Button variant="primary">
                <RotateCcw aria-hidden />
                Re-run job
              </Button>
            </>
          }
        />
      </div>
      <KeyValueList>
        <KeyValue label="Commit">c4e8f12 Fix redirect loop after Stripe checkout</KeyValue>
        <KeyValue label="Branch">fix/checkout-redirect</KeyValue>
        <KeyValue label="Runner">
          <RunnerLabel className="leading-s text-neutral-10">runplane-8cpu</RunnerLabel>
        </KeyValue>
        <KeyValue label="Server">fsn1-ax52-a</KeyValue>
        <KeyValue label="Queued">4s</KeyValue>
        <KeyValue label="Duration">6m 48s</KeyValue>
        <KeyValue label="Triggered">PR #412 by mira-k</KeyValue>
      </KeyValueList>
      <PageHeader title="Servers" count={6} actions={<Button variant="primary">Add server</Button>} />
      <div className="grid grid-cols-2 gap-xl md:grid-cols-5">
        <Stat label="Jobs" value="1,284" />
        <Stat label="vCPU in use" value={36} total={80} />
        <Stat label="Median queue time" value="2.8s" />
        <Stat label="Failure rate" value={null} />
        <Stat label="Cache hit rate" value={null} />
      </div>
      <SectionHeader title="Recent jobs" action={<SectionLink to="." chevron>All jobs</SectionLink>} />
      <SectionHeader title="Servers" action={<SectionButton>Add server</SectionButton>} level={3} />
      <EmptyState
        title="No jobs yet"
        actions={
          <>
            <CopyButton text="runplane-4cpu" variant="primary">
              Copy label
            </CopyButton>
            <Button>See all labels</Button>
          </>
        }
        aside={<CodeBlock title=".github/workflows/ci.yml" code={workflowDiff('runplane-4cpu')} />}
      >
        Change runs-on in any workflow to send that job to your servers. Jobs show up here as soon as GitHub queues them.
      </EmptyState>
    </Section>
  )
}

function Code() {
  return (
    <Section title="CodeBlock, CopyButton">
      <div className="grid grid-cols-1 gap-xl md:grid-cols-2">
        <CodeBlock title=".github/workflows/ci.yml" code={workflowDiff('runplane-4cpu')} copyable />
        <CodeBlock title="runplane.toml" code={'[server]\nlisten = ":8484"\npublic_url = "https://ci.kestrel.dev"'} lineNumbers />
      </div>
      <CodeBlock copyable code={['curl -fsSL https://ci.kestrel.dev/install.sh | sudo sh -s -- \\', '  --token rp_srv_8Fq2nL7xVb3kR9tW']} />
      <Specimen label="CopyButton">
        <CopyButton text="runplane-8cpu" />
        <CopyButton text="runplane-8cpu">Copy label</CopyButton>
        <Button>
          <ExternalLink aria-hidden />
          Edit on GitHub
        </Button>
      </Specimen>
    </Section>
  )
}

// "Jobs per hour" of the Overview design: bar heights and failed caps in pixels of a 161px tallest bar.
const barHeights = [128, 138, 117, 76, 50, 37, 27, 22, 15, 11, 7, 17, 38, 10, 7, 14, 29, 59, 118, 157, 161, 115, 145, 79]
const capHeights = [8, 9, 7, 4, 4, 4, 0, 4, 0, 0, 0, 4, 9, 0, 0, 0, 4, 4, 7, 8, 9, 6, 7, 4]

const jobBars: BarChartBar[] = barHeights.map((height, index) => {
  const value = Math.round((height / 161) * 131)
  const part = capHeights[index] ? Math.max(1, Math.round((capHeights[index]! / 161) * 131 * 0.8)) : 0
  const hour = (15 + index) % 24
  const label = `${hours(hour)} – ${hours((hour + 1) % 24)}`
  return { label, value, part, detail: `${value} jobs · ${part} failed` }
})

// CPU and memory of the Job detail design, as y pixels of a 64px plot over 6:48.
const cpuYs = [43.3, 42.6, 17.7, 16.1, 13.5, 12.7, 40.8, 33.2, 35.9, 37.7, 38.9, 34.1, 35.3, 40.9, 6.4, 4.6, 5.8, 9.7, 1.9, 10.3, 1.7, 7.2, 4.6, 9.8, 7, 5.9, 9.9, 7.9, 10.7, 9.1, 6.7, 5.3, 3.7, 10.1, 6.9, 11, 4.5, 1.7, 7, 7, 6.9, 8.8, 3.9, 8.8, 2.7, 2, 3.9, 5.8, 8.2, 9.3, 4.5, 8.8, 11.4, 5.5, 10.6, 6.8, 5.5, 10.2, 2.3, 11.3, 4.3, 4.8, 9.2, 9.8, 10.7, 6, 2.9, 6.5, 8.7, 10.9, 9.2, 3.4, 9.8, 5.1, 7.4, 6.6, 11.4, 6.9, 8.1, 4.2, 56.7, 56.4]
const memoryYs = [61.6, 60.8, 58.4, 57.4, 56.4, 55.4, 54.8, 54.8, 54.8, 54.8, 54.8, 54.8, 54.8, 54.8, 53.2, 52.7, 51.5, 51.6, 49.6, 49.8, 49.6, 48.4, 48.3, 46.5, 47.1, 46.4, 45, 45.1, 43.6, 43.1, 43.5, 42.6, 41.6, 41.7, 41.3, 40.2, 39.2, 39.1, 38.2, 37.1, 37.1, 36.8, 36.4, 34.8, 33.9, 33.4, 33.2, 33.6, 32.4, 31.8, 30.6, 29.6, 30, 29.3, 27.8, 27.4, 27.4, 26.3, 26.5, 26.1, 24.6, 23.7, 23.2, 23.9, 23.3, 21.4, 21.9, 21.4, 20.1, 19.7, 19.2, 18.9, 18.5, 18.4, 18.4, 18.4, 18.4, 18.4, 18.4, 18.4, 52, 52]
const jobLength = 408_000

function samples(ys: readonly number[], max: number): LinePoint[] {
  return ys.map((y, index) => ({ x: (index / (ys.length - 1)) * jobLength, y: ((64 - y) / 64) * max }))
}

function Charts() {
  const cpu = useMemo(() => samples(cpuYs, 100), [])
  const memory = useMemo(() => samples(memoryYs, 16384), [])
  return (
    <Section title="BarChart, LineChart">
      <div className="flex flex-col gap-xl lg:flex-row">
        <Card className="min-w-0 lg:grow-3 lg:basis-0">
          <div className="flex items-center justify-between gap-m">
            <h3 className="text-m font-heavy tracking-l text-neutral-10">Jobs per hour</h3>
            <ChartLegend
              items={[
                { label: 'Succeeded', color: 'var(--neutral-5)' },
                { label: 'Failed', color: 'var(--neutral-10)' },
              ]}
            />
          </div>
          <BarChart
            aria-label="Jobs per hour"
            bars={jobBars}
            ticks={[
              { at: 0, label: '15:00' },
              { at: 6, label: '21:00' },
              { at: 12, label: '03:00' },
              { at: 18, label: '09:00' },
            ]}
            endLabel="Now"
            valueName="Jobs"
            partName="Failed"
          />
        </Card>
        <Card className="min-w-0 lg:grow-2 lg:basis-0">
          <h3 className="text-m font-heavy tracking-l text-neutral-10">No data</h3>
          <BarChart aria-label="Jobs per hour" bars={jobBars.map((bar) => ({ ...bar, value: 0, part: 0 }))} ticks={[]} valueName="Jobs" partName="Failed" />
        </Card>
      </div>
      <div className="flex flex-col gap-xl md:flex-row">
        <Card className="min-w-0 grow basis-0 gap-m">
          <ChartHeader title="CPU" value="98% peak" detail="64% avg of 8 vCPU" />
          <LineChart
            aria-label="CPU use"
            valueName="CPU"
            points={cpu}
            max={100}
            marker={{ x: 70_000, label: 'Run Playwright' }}
            startLabel="0:00"
            endLabel="6:48"
            formatX={clock}
            formatY={(y) => `${Math.round(y)}%`}
          />
        </Card>
        <Card className="min-w-0 grow basis-0 gap-m">
          <ChartHeader title="Memory" value="11.4 GB peak" detail="of 16 GB" />
          <LineChart
            aria-label="Memory use"
            valueName="Memory"
            points={memory}
            max={16384}
            limitLine
            marker={{ x: 70_000, label: 'Run Playwright' }}
            startLabel="0:00"
            endLabel="6:48"
            formatX={clock}
            formatY={(y) => `${(y / 1024).toFixed(1)} GB`}
          />
        </Card>
      </div>
      <Card className="gap-m md:max-w-[50%]">
        <ChartHeader title="Marker near the start" value="98% peak" detail="the label moves past 0:00" />
        <LineChart
          aria-label="CPU use, step at the start"
          valueName="CPU"
          points={cpu}
          max={100}
          marker={{ x: 2_000, label: 'Run Playwright' }}
          startLabel="0:00"
          endLabel="6:48"
          formatX={clock}
          formatY={(y) => `${Math.round(y)}%`}
        />
      </Card>
    </Section>
  )
}

function ChartHeader({ title, value, detail }: { title: string; value: string; detail: string }) {
  return (
    <div className="flex items-baseline justify-between gap-m">
      <h3 className="text-m font-heavy tracking-l text-neutral-10">{title}</h3>
      <div className="flex items-baseline gap-xs">
        <span className="text-s font-medium text-neutral-10">{value}</span>
        <span className="text-xs text-neutral-7">{detail}</span>
      </div>
    </div>
  )
}

const designLog = [
  '2026-11-04T10:12:01.0000000Z ##[group]Run pnpm install --frozen-lockfile',
  '2026-11-04T10:12:01.1000000Z Lockfile is up to date, resolution step is skipped',
  '2026-11-04T10:12:19.0000000Z \x1b[32mDone in 18s\x1b[0m',
  '2026-11-04T10:12:19.1000000Z ##[endgroup]',
  '2026-11-04T10:12:19.2000000Z ##[command]pnpm exec playwright test --project=webkit',
  '> web@0.0.0 test:e2e /home/runner/work/web/web',
  '> playwright test --project=webkit',
  '',
  'Running 148 tests using 8 workers',
  '',
  '  \x1b[32m✓\x1b[0m  checkout/cart.spec.ts:14:3 › adds item to cart \x1b[90m(2.1s)\x1b[0m',
  '  \x1b[32m✓\x1b[0m  checkout/cart.spec.ts:38:3 › applies promo code \x1b[90m(1.8s)\x1b[0m',
  '  \x1b[32m✓\x1b[0m  auth/login.spec.ts:9:3 › signs in with email \x1b[90m(1.2s)\x1b[0m',
  '  \x1b[31m✘\x1b[0m  checkout/redirect.spec.ts:22:3 › returns to order page after payment \x1b[90m(30.0s)\x1b[0m',
  '',
  '  1) [webkit] › checkout/redirect.spec.ts:22:3 › returns to order page after payment',
  '',
  '##[error]Timed out 30000ms waiting for expect(page).toHaveURL(/\\/orders\\/\\w+/)',
  '',
  '    Expected pattern: \x1b[32m/\\/orders\\/\\w+/\x1b[0m',
  '    Received string:  \x1b[31m"http://localhost:3000/checkout/return?session_id=cs_test_a1"\x1b[0m',
  '',
  "      29 |   await page.getByRole('button', { name: 'Pay now' }).click();",
  '    \x1b[31m\x1b[1m>\x1b[22m\x1b[39m 30 |   await expect(page).toHaveURL(/\\/orders\\/\\w+/);',
  '',
  '  \x1b[31m1 failed\x1b[39m',
  '  \x1b[32m147 passed\x1b[39m (5.4m)',
  '##[error]Process completed with exit code 1.',
].join('\n')

/** The 100,000-line log, then 20 more lines each 100ms, as a running job streams them. */
function useLiveLog(running: boolean): string {
  const [log, setLog] = useState('')
  useEffect(() => {
    if (!running) return
    let next = 0
    setLog(bigLog(100_000))
    const timer = setInterval(() => {
      const chunk = Array.from({ length: 20 }, () => (++next % 97 === 0 ? `Error: flaky test ${next}` : `live line ${next}`))
      setLog((previous) => `${previous}\n${chunk.join('\n')}`)
    }, 100)
    return () => clearInterval(timer)
  }, [running])
  return log
}

/** A log of `count` lines with folded groups, colors, and an error every 5,000 lines. */
function bigLog(count: number): string {
  const lines: string[] = []
  for (let i = 0; i < count; i++) {
    if (i % 250 === 0) lines.push(`##[group]Batch ${i / 250 + 1}`)
    else if (i % 250 === 249) lines.push('##[endgroup]')
    else if (i % 5000 === 4100) lines.push(`##[error]Test ${i} failed: expected 200, got 502`)
    else lines.push(`\x1b[90m[${String(i).padStart(6, '0')}]\x1b[0m \x1b[32mok\x1b[0m test_${i}: payload ${'x'.repeat(i % 60)}`)
  }
  return lines.join('\n')
}

function Logs() {
  const [kind, setKind] = useState<'design' | 'big' | 'live'>('design')
  const live = useLiveLog(kind === 'live')
  const log = useMemo(() => (kind === 'big' ? bigLog(100_000) : kind === 'live' ? live : designLog), [kind, live])
  return (
    <Section title="LogViewer">
      <Specimen label="Log">
        <SegmentedControl
          aria-label="Log size"
          value={kind}
          onValueChange={setKind}
          options={[
            { value: 'design', label: 'Job detail' },
            { value: 'big', label: '100,000 lines' },
            { value: 'live', label: 'Live, following' },
          ]}
        />
      </Specimen>
      <LogViewer log={log} follow={kind === 'live'} title="Run Playwright (webkit)" meta="5m 32s" downloadName="e2e-webkit.log" maxHeight="480px" />
      <LogViewer log="" title="Upload report" meta="Skipped" />
    </Section>
  )
}

/** "15:00" for hour 15. */
function hours(hour: number): string {
  return `${String(hour).padStart(2, '0')}:00`
}
