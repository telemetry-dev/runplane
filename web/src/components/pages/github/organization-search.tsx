import { keepPreviousData, useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { errorMessage } from '~/api/client'
import { Avatar } from '~/components/ui/avatar'
import { Combobox, ComboboxInput, ComboboxItem, ComboboxPopup } from '~/components/ui/combobox'
import { useDebouncedValue } from '~/lib/use-debounced-value'
import { findOrganizations } from './organizations'

type OrganizationSearchProps = {
  /** The login of the selected organization. */
  value: string | null
  onValueChange: (login: string | null) => void
  disabled: boolean
}

/**
 * Searches GitHub as the user types and lists the matching organizations. Only
 * a selected organization counts, so the app cannot go to a name that GitHub
 * does not know.
 */
export function OrganizationSearch({ value, onValueChange, disabled }: OrganizationSearchProps) {
  const [text, setText] = useState('')
  const term = useDebouncedValue(text.trim(), 300)
  const search = useQuery({
    queryKey: ['github-organizations', term.toLowerCase()],
    queryFn: ({ signal }) => findOrganizations(term, signal),
    enabled: term !== '',
    staleTime: 5 * 60_000,
    retry: false,
    placeholderData: keepPreviousData,
  })
  const orgs = term === '' ? [] : (search.data ?? [])

  return (
    <Combobox
      items={orgs.map((org) => org.login)}
      filter={null}
      value={value}
      onValueChange={(login: string | null) => onValueChange(login)}
      onInputValueChange={setText}
      autoHighlight
      disabled={disabled}
    >
      <ComboboxInput placeholder="Search GitHub" autoComplete="off" spellCheck={false} autoCapitalize="none" />
      <ComboboxPopup
        empty={
          text.trim() === ''
            ? 'Type the name of the organization.'
            : text.trim() !== term || search.isFetching
              ? 'Searching GitHub…'
              : search.isError
                ? errorMessage(search.error)
                : 'GitHub has no organization with this name.'
        }
      >
        {(login: string) => (
          <ComboboxItem key={login} value={login}>
            <span className="flex items-center gap-xs">
              <Avatar name={login} src={orgs.find((org) => org.login === login)?.avatar_url} className="size-(--line-s)" />
              {login}
            </span>
          </ComboboxItem>
        )}
      </ComboboxPopup>
    </Combobox>
  )
}
