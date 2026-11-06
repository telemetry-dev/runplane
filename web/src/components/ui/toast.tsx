import { Toast } from '@base-ui/react/toast'
import { X } from 'lucide-react'
import { cx } from '~/lib/cx'
import { buttonClassName, IconButton } from './button'
import { Spinner } from './spinner'

// Toasts of gui/components/runplane/toast.md. Mount <Toaster /> once near the
// root, then show a toast from anywhere:
//
//   toastManager.add({ title: 'Server removed' })
//   toastManager.promise(save(), { loading: 'Saving…', success: 'Saved', error: errorMessage })
//
// `type: 'loading'` shows a spinner. An `actionProps` ({ children, onClick })
// adds an action button, for example Undo.

/** The app's one toast queue. Toaster renders it. */
export const toastManager = Toast.createToastManager()

/** The timeout of a toast pauses while it is hovered or focused. */
export function Toaster() {
  return (
    <Toast.Provider toastManager={toastManager} timeout={5000} limit={4}>
      <Toast.Portal>
        <Toast.Viewport className="fixed right-l bottom-l z-30 flex w-[min(360px,calc(100vw-var(--space-xl)))] flex-col gap-s outline-none">
          <ToastList />
        </Toast.Viewport>
      </Toast.Portal>
    </Toast.Provider>
  )
}

function ToastList() {
  const { toasts } = Toast.useToastManager()
  // The newest toast is first; show it at the bottom, next to the screen edge.
  return toasts.toReversed().map((toast) => (
    <Toast.Root
      key={toast.id}
      toast={toast}
      swipeDirection={['right', 'down']}
      className={cx(
        'relative rounded-m bg-neutral-1 p-m text-s text-neutral-10 select-none dark:bg-neutral-2',
        'shadow-[0_0_0_1px_var(--neutral-4),var(--shadow-m)]',
        '[transform:translate(var(--toast-swipe-movement-x),var(--toast-swipe-movement-y))]',
        'transition-[transform,opacity] duration-(--motion-large-duration) ease-large data-swiping:duration-0',
        'data-starting-style:[transform:translateY(100%)] data-starting-style:opacity-0',
        'data-ending-style:opacity-0 data-limited:hidden',
        'data-ending-style:data-[swipe-direction=right]:[transform:translateX(calc(var(--toast-swipe-movement-x)+100%))]',
        'data-ending-style:data-[swipe-direction=down]:[transform:translateY(calc(var(--toast-swipe-movement-y)+100%))]',
      )}
    >
      <Toast.Content className="flex flex-col gap-xs">
        <div className="flex items-start gap-xs pr-xl">
          {toast.type === 'loading' && <Spinner size={14} className="mt-[2px]" />}
          <div className="flex min-w-0 flex-1 flex-col gap-xxs">
            <Toast.Title className="font-medium text-neutral-10" />
            <Toast.Description className="text-neutral-7" />
          </div>
        </div>
        {toast.actionProps && (
          <div className="flex justify-end gap-xs">
            <Toast.Action className={buttonClassName('secondary')} />
          </div>
        )}
      </Toast.Content>
      <Toast.Close render={<IconButton label="Dismiss" className="absolute top-s right-s p-xxs [&_svg]:size-[14px]" />}>
        <X aria-hidden />
      </Toast.Close>
    </Toast.Root>
  ))
}
