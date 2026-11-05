import { useQuery } from '@tanstack/react-query'
import { useCallback, useEffect, useRef, useState } from 'react'
import { facetsQuery } from '~/api/jobs'
import { FilterSelect } from '~/components/app/filter-select'
import { SearchInput } from '~/components/ui/input'
import { defaultRange, rangeOptions, statusOptions, type JobsSearch } from './jobs-search'

type JobsToolbarProps = {
  search: JobsSearch
  /** Applies a change to the search params. `typing` replaces the history entry instead of adding one. */
  onChange: (change: Partial<JobsSearch>, typing?: boolean) => void
}

/** Search and filters above the Jobs table. */
export function JobsToolbar({ search, onChange }: JobsToolbarProps) {
  const facets = useQuery({ ...facetsQuery, staleTime: 60_000 }).data
  const setQuery = useCallback((q: string) => onChange({ q: q || undefined }, true), [onChange])
  return (
    <div className="@container">
      <div role="search" className="flex flex-wrap items-center gap-xs rounded-m bg-neutral-2 p-xs">
        <QueryInput value={search.q ?? ''} onChange={setQuery} />
        <FilterSelect
          label="Status"
          allLabel="Any"
          value={search.status ?? null}
          onValueChange={(status) => onChange({ status: status ?? undefined })}
          options={statusOptions}
        />
        <FilterSelect
          label="Repository"
          allLabel="All"
          value={search.repository ?? null}
          onValueChange={(repository) => onChange({ repository: repository ?? undefined })}
          options={(facets?.repositories ?? []).map((name) => ({ value: name, label: name }))}
        />
        <FilterSelect
          label="Runner"
          allLabel="All"
          value={search.pool ?? null}
          onValueChange={(pool) => onChange({ pool: pool ?? undefined })}
          options={(facets?.pools ?? []).map((pool) => ({ value: pool.id, label: pool.name }))}
        />
        <FilterSelect
          label="Server"
          allLabel="All"
          value={search.server ?? null}
          onValueChange={(server) => onChange({ server: server ?? undefined })}
          options={(facets?.servers ?? []).map((server) => ({ value: server.id, label: server.name }))}
        />
        <FilterSelect
          aria-label="Time range"
          value={search.range ?? defaultRange}
          onValueChange={(range) => onChange({ range: range === defaultRange || range === null ? undefined : range })}
          options={rangeOptions}
        />
      </div>
    </div>
  )
}

/** The search field. It sends its text 300 ms after the last key press. */
function QueryInput({ value, onChange }: { value: string; onChange: (value: string) => void }) {
  const [text, setText] = useState(value)
  // The last text this field sent. A different value comes from outside, such as Back or "Clear filters".
  const sent = useRef(value)

  useEffect(() => {
    if (value === sent.current) return
    sent.current = value
    setText(value)
  }, [value])

  useEffect(() => {
    if (text.trim() === sent.current) return
    const timer = setTimeout(() => {
      sent.current = text.trim()
      onChange(text.trim())
    }, 300)
    return () => clearTimeout(timer)
  }, [text, onChange])

  return (
    <SearchInput
      aria-label="Search jobs"
      placeholder="Search job, branch or commit"
      value={text}
      onChange={(event) => setText(event.target.value)}
      className="basis-full @min-[720px]:min-w-[200px] @min-[720px]:grow @min-[720px]:basis-0"
    />
  )
}
