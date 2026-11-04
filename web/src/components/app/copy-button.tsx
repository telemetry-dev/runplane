import { Check, Copy } from 'lucide-react'
import { useEffect, useState, type ReactNode } from 'react'
import { Button, IconButton, type ButtonVariant } from '~/components/ui/button'
import { toastManager } from '~/components/ui/toast'

type CopyButtonProps = {
  /** The text that goes to the clipboard. */
  text: string
  /** Visible label. Without it the button is a 16px icon with "Copy" as its name. */
  children?: ReactNode
  variant?: ButtonVariant
  className?: string
}

/** Copies text and shows a check for a moment, as the install command and "Copy label" buttons. */
export function CopyButton({ text, children, variant = 'secondary', className }: CopyButtonProps) {
  const [copied, setCopied] = useState(false)

  useEffect(() => {
    if (!copied) return
    const timer = setTimeout(() => setCopied(false), 1500)
    return () => clearTimeout(timer)
  }, [copied])

  async function copy() {
    try {
      await copyText(text)
      setCopied(true)
    } catch {
      toastManager.add({ title: 'Could not copy', description: 'The browser did not allow it. Select the text and copy it by hand.' })
    }
  }

  const icon = copied ? <Check aria-hidden /> : <Copy aria-hidden />
  if (!children) {
    return (
      <IconButton label={copied ? 'Copied' : 'Copy'} onClick={() => void copy()} className={className}>
        {icon}
      </IconButton>
    )
  }
  return (
    <Button variant={variant} onClick={() => void copy()} className={className}>
      {icon}
      {children}
      <span className="sr-only" aria-live="polite">
        {copied ? 'Copied' : ''}
      </span>
    </Button>
  )
}

/**
 * Writes text to the clipboard, or rejects when the browser refuses. The
 * Clipboard API needs a secure context, and a fresh install is often reached
 * over plain HTTP, so fall back to a selected textarea and the copy command there.
 */
export async function copyText(text: string): Promise<void> {
  if (window.isSecureContext && navigator.clipboard) {
    await navigator.clipboard.writeText(text)
    return
  }
  const area = document.createElement('textarea')
  area.value = text
  area.setAttribute('readonly', '')
  area.style.position = 'fixed'
  area.style.opacity = '0'
  document.body.append(area)
  area.select()
  try {
    // The command fails without a recent click, as after an awaited request.
    if (!document.execCommand('copy')) throw new Error('The browser did not allow the copy.')
  } finally {
    area.remove()
  }
}
