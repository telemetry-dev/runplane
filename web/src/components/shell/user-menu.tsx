import { Form } from '@base-ui/react/form'
import { useQuery } from '@tanstack/react-query'
import { KeyRound, LogOut } from 'lucide-react'
import { useState } from 'react'
import { $api, api, isApiError } from '~/api/client'
import { useFormProblems } from '~/api/form-problems'
import { sessionQuery, statusQuery } from '~/api/session'
import { Avatar } from '~/components/ui/avatar'
import { Button } from '~/components/ui/button'
import { Dialog, DialogClose, DialogFooter, DialogPopup } from '~/components/ui/dialog'
import { Field, Input } from '~/components/ui/field'
import {
  Menu,
  MenuGroup,
  MenuGroupLabel,
  MenuItem,
  MenuNote,
  MenuPopup,
  MenuRadioGroup,
  MenuRadioItem,
  MenuSeparator,
  MenuTrigger,
} from '~/components/ui/menu'
import { useThemeMode } from '~/lib/theme-mode'
import { useFrameSubscribed } from './pending'

/** The user row at the bottom of the sidebar; opens theme mode, password change, and sign out. */
export function UserMenu() {
  const subscribed = useFrameSubscribed()
  const user = useQuery({ ...sessionQuery, subscribed }).data
  const name = user?.name || user?.email || ''
  const version = useQuery({ ...statusQuery, subscribed }).data?.version
  const [mode, setMode] = useThemeMode()
  const [passwordOpen, setPasswordOpen] = useState(false)

  async function signOut() {
    // A 401 means the session has already ended.
    await api.DELETE('/api/session').catch((error: unknown) => {
      if (!isApiError(error, 401)) throw error
    })
    // A full load drops all cached data and the event stream of this user.
    location.assign('/login')
  }

  return (
    <>
      <Menu>
        <MenuTrigger className="group -my-xxs flex items-center gap-s rounded-s px-xs py-xxs text-left transition-colors hover:bg-neutral-3 data-popup-open:bg-neutral-3">
          <Avatar name={name} className="group-hover:bg-neutral-1 group-data-popup-open:bg-neutral-1" />
          <span className="min-w-0 flex-1 truncate text-s text-neutral-8">{name}</span>
          {version && (
            <span title={version} className="text-xs text-neutral-6">
              {shortVersion(version)}
            </span>
          )}
        </MenuTrigger>
        <MenuPopup side="top">
          {user && <MenuNote>{user.email}</MenuNote>}
          <MenuGroup>
            <MenuGroupLabel>Theme</MenuGroupLabel>
            <MenuRadioGroup value={mode} onValueChange={setMode}>
              <MenuRadioItem value="system">System</MenuRadioItem>
              <MenuRadioItem value="light">Light</MenuRadioItem>
              <MenuRadioItem value="dark">Dark</MenuRadioItem>
            </MenuRadioGroup>
          </MenuGroup>
          <MenuSeparator />
          <MenuItem icon={<KeyRound />} onClick={() => setPasswordOpen(true)}>
            Change password
          </MenuItem>
          <MenuItem icon={<LogOut />} onClick={() => void signOut()}>
            Sign out
          </MenuItem>
        </MenuPopup>
      </Menu>
      <Dialog open={passwordOpen} onOpenChange={setPasswordOpen}>
        <DialogPopup title="Change password">
          <ChangePasswordForm onDone={() => setPasswordOpen(false)} />
        </DialogPopup>
      </Dialog>
    </>
  )
}

// Lives inside the popup, so closing the dialog drops the form state and the last error.
function ChangePasswordForm({ onDone }: { onDone: () => void }) {
  const update = $api.useMutation('patch', '/api/me', { onSuccess: onDone })
  const problems = useFormProblems(update.error)

  return (
    <Form
      className="flex flex-col gap-l"
      errors={problems.fields}
      onFormSubmit={(values) =>
        update.mutate({ body: { current_password: values.current_password, new_password: values.new_password } })
      }
    >
      <Field name="current_password" label="Current password">
        <Input type="password" autoComplete="current-password" required />
      </Field>
      <Field name="new_password" label="New password">
        <Input type="password" autoComplete="new-password" required />
      </Field>
      <Field
        name="confirm_password"
        label="Repeat new password"
        validate={(value, values) => (value === values.new_password ? null : 'The passwords are not the same.')}
      >
        <Input type="password" autoComplete="new-password" required />
      </Field>
      {problems.alert && (
        <p role="alert" className="text-s text-error">
          {problems.alert}
        </p>
      )}
      <DialogFooter>
        <DialogClose render={<Button />}>Cancel</DialogClose>
        <Button type="submit" variant="primary" disabled={update.isPending}>
          Change password
        </Button>
      </DialogFooter>
    </Form>
  )
}

/** "v1.2.0 (3f2a9c1)" and "1.2.0" both give "v1.2.0"; other names such as "dev" stay. */
function shortVersion(version: string): string {
  const name = version.split(' ')[0]!
  return /^\d/.test(name) ? `v${name}` : name
}
