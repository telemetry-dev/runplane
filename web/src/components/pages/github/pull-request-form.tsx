import { ExternalLink } from 'lucide-react'
import { useState, type FormEvent } from 'react'
import { $api, errorMessage, isApiError } from '~/api/client'
import { StatusIcon } from '~/components/app/status-icon'
import { FormAlert } from '~/components/pages/setup/parts'
import { Button, buttonClassName } from '~/components/ui/button'
import { Combobox, ComboboxInput, ComboboxItem, ComboboxPopup } from '~/components/ui/combobox'
import { Select, SelectItem, SelectPopup, SelectTrigger } from '~/components/ui/select'
import { ExternalTextLink } from './parts'

type PullRequestFormProps = {
  /** owner/name of the repositories whose jobs runplane takes. */
  repositories: string[]
  /** The labels of the enabled pools. With more than one, the form shows a label select. */
  labels: string[]
  defaultLabel: string
  /** The permissions page of the GitHub App. An app from before runplane opened pull requests must get write access there first. */
  permissionsUrl: string | undefined
}

/**
 * runplane sets runs-on to a label in each workflow of a repository and opens
 * a pull request. The form stays after a pull request opens, so the next
 * repository can follow.
 */
export function PullRequestForm({ repositories, labels, defaultLabel, permissionsUrl }: PullRequestFormProps) {
  const [repository, setRepository] = useState<string | null>(repositories.length === 1 ? (repositories[0] ?? null) : null)
  const [label, setLabel] = useState(defaultLabel)
  const [unselected, setUnselected] = useState(false)
  const create = $api.useMutation('post', '/api/github/pull-requests')

  function submit(event: FormEvent) {
    event.preventDefault()
    if (repository) create.mutate({ body: { repository, label } })
    else setUnselected(true)
  }

  return (
    <form onSubmit={submit} className="flex flex-col gap-s rounded-m bg-neutral-2 p-m">
      <div className="flex flex-wrap items-center gap-s">
        <Combobox
          items={repositories}
          value={repository}
          onValueChange={(name: string | null) => {
            setRepository(name)
            setUnselected(false)
            if (create.isError) create.reset()
          }}
          autoHighlight
          disabled={create.isPending}
        >
          <ComboboxInput
            aria-label="Repository"
            placeholder="Search repositories"
            autoComplete="off"
            spellCheck={false}
            className="w-auto min-w-[240px] flex-1"
          />
          <ComboboxPopup empty="No repository matches.">
            {(name: string) => (
              <ComboboxItem key={name} value={name}>
                {name}
              </ComboboxItem>
            )}
          </ComboboxPopup>
        </Combobox>
        {labels.length > 1 && (
          <Select
            items={labels.map((name) => ({ value: name, label: name }))}
            value={label}
            onValueChange={(name) => name && setLabel(name)}
            disabled={create.isPending}
          >
            <SelectTrigger aria-label="Label" className="w-[160px]" />
            <SelectPopup>
              {labels.map((name) => (
                <SelectItem key={name} value={name}>
                  {name}
                </SelectItem>
              ))}
            </SelectPopup>
          </Select>
        )}
        <Button type="submit" variant="primary" loading={create.isPending}>
          Open pull request
        </Button>
      </div>
      <p className="text-s text-neutral-7">
        runplane sets runs-on to {label} for each job on ubuntu-latest or ubuntu-24.04 in the repository, then opens a pull request for you to
        review.
      </p>
      {unselected && <FormAlert>Select a repository in the list.</FormAlert>}
      {create.error && (
        <FormAlert>
          {errorMessage(create.error)}
          {isApiError(create.error, 403) && permissionsUrl && (
            <>
              {' '}
              <ExternalTextLink href={permissionsUrl}>
                App permissions
                <ExternalLink size={14} aria-hidden />
                <span className="sr-only"> (opens in a new tab)</span>
              </ExternalTextLink>
            </>
          )}
        </FormAlert>
      )}
      {create.data && (
        <div className="flex flex-wrap items-center justify-between gap-x-m gap-y-s">
          <p role="status" className="flex min-w-0 items-center gap-s text-s font-medium text-neutral-10">
            <StatusIcon status="succeeded" />
            <span className="min-w-0">
              Pull request #{create.data.number} is open in {create.variables?.body.repository}
            </span>
          </p>
          <a href={create.data.html_url} target="_blank" rel="noreferrer" className={buttonClassName('secondary')}>
            View on GitHub
            <ExternalLink aria-hidden />
            <span className="sr-only"> (opens in a new tab)</span>
          </a>
        </div>
      )}
    </form>
  )
}
