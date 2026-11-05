import { ExternalLink } from 'lucide-react'
import { $api, errorMessage, type Schemas } from '~/api/client'
import { copyText } from '~/components/app/copy-button'
import { Button, buttonClassName } from '~/components/ui/button'
import { Combobox, ComboboxInput, ComboboxItem, ComboboxPopup } from '~/components/ui/combobox'
import { GithubIcon } from '~/components/ui/icons'
import { Spinner } from '~/components/ui/spinner'

export type Account = Schemas['GitHubAccount']

/**
 * A GitHub sign-in that lists the accounts for the app. It uses the device
 * flow, so GitHub never sends the browser back to this instance, which may
 * have no public URL. The result is read at the interval that GitHub asks for.
 */
export function useAccountSignIn() {
  const start = $api.useMutation('post', '/api/github/sign-in')
  const id = start.data?.id
  const result = $api.useQuery(
    'get',
    '/api/github/sign-in/{id}',
    { params: { path: { id: id ?? '' } } },
    {
      enabled: id !== undefined,
      retry: false,
      refetchInterval: (query) => {
        const data = query.state.data
        if (query.state.status === 'error' || (data && data.status !== 'pending')) return false
        return Math.max(data?.interval ?? start.data?.interval ?? 5, 1) * 1000
      },
    },
  )
  return { start, result, accounts: result.data?.status === 'done' ? result.data.accounts : undefined }
}

type AccountPickerProps = {
  signIn: ReturnType<typeof useAccountSignIn>
  /** ID of the visible "Install on" label. */
  labelId: string
  value: Account | undefined
  onValueChange: (login: string) => void
  disabled: boolean
}

/** "Install on" with a sign-in: the button, then the device code, then the accounts. */
export function AccountPicker({ signIn: { start, result, accounts }, labelId, value, onValueChange, disabled }: AccountPickerProps) {
  if (accounts) {
    return (
      <>
        <Combobox
          items={accounts.map((account) => account.login)}
          value={value?.login ?? null}
          onValueChange={(login) => login && onValueChange(login)}
          disabled={disabled}
        >
          <ComboboxInput aria-labelledby={labelId} placeholder="Choose an account" />
          <ComboboxPopup empty="No accounts match.">
            {(login: string) => (
              <ComboboxItem key={login} value={login}>
                {login} <span className="text-neutral-7">{kind(accounts.find((account) => account.login === login)!)}</span>
              </ComboboxItem>
            )}
          </ComboboxPopup>
        </Combobox>
        {value && <p className="text-s text-neutral-7">{scope(value)}</p>}
      </>
    )
  }

  const code = start.data
  const ended = start.isError || result.isError || result.data?.status === 'expired' || result.data?.status === 'denied'
  if (!code || ended) {
    return (
      <div className="flex flex-col items-start gap-s">
        {ended && (
          <p role="alert" className="text-s text-neutral-8">
            {start.isError
              ? errorMessage(start.error)
              : result.isError
                ? errorMessage(result.error)
                : result.data?.status === 'denied'
                  ? 'The sign-in was cancelled on GitHub.'
                  : 'The code expired.'}
          </p>
        )}
        <Button onClick={() => start.mutate({})} loading={start.isPending} disabled={disabled}>
          {!start.isPending && <GithubIcon aria-hidden />}
          {ended ? 'Sign in again' : 'Sign in with GitHub'}
        </Button>
        {!ended && <p className="text-s text-neutral-7">runplane lists your organizations and your personal account. It does not keep the GitHub token.</p>}
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-s">
      <p className="text-s text-neutral-8">Type this code on GitHub, then approve runplane.</p>
      <p className="font-code text-l tracking-xl text-neutral-10 select-all">{code.user_code}</p>
      <div className="flex flex-wrap items-center gap-x-m gap-y-s">
        <a
          href={code.verification_uri}
          target="_blank"
          rel="noreferrer"
          // A failed copy is fine: the code is on the page.
          onClick={() => void copyText(code.user_code).catch(() => {})}
          className={buttonClassName('secondary')}
        >
          <ExternalLink aria-hidden />
          Copy code and open GitHub
        </a>
        <p role="status" className="flex items-center gap-s text-s text-neutral-7">
          <Spinner size={14} />
          Waiting for GitHub
        </p>
      </div>
    </div>
  )
}

function kind(account: Account): string {
  if (account.type === 'user') return 'Personal account'
  return account.owner ? 'Organization' : 'Organization · member'
}

/** Which repositories can use the runners, or why the app may fail on that account. */
function scope(account: Account): string {
  if (account.type === 'user') return 'Only repositories you own can use your runners.'
  if (account.owner) return 'Every repository in the organization can use your runners.'
  return `You are not an owner of ${account.login}. GitHub lets only its owners and app managers make the app.`
}
