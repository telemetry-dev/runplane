import { useEffect, useSyncExternalStore } from 'react'

export type ThemeMode = 'system' | 'light' | 'dark'

const storageKey = 'runplane.theme'

/**
 * Inline script for <head>. It sets the dark class before the first paint, so a
 * dark-mode user never sees a light flash. It runs as a string in the page, so
 * applyMode must not use anything outside its own body.
 */
export const themeModeScript = `(${applyMode.toString()})(${JSON.stringify(storageKey)})`

/** The saved mode and a setter. Also follows the OS setting while the mode is System. */
export function useThemeMode(): [ThemeMode, (mode: ThemeMode) => void] {
  const mode = useSyncExternalStore(subscribe, readMode, () => 'system' as const)

  useEffect(() => {
    if (mode !== 'system') return
    const query = matchMedia('(prefers-color-scheme: dark)')
    const onChange = () => applyMode(storageKey)
    query.addEventListener('change', onChange)
    return () => query.removeEventListener('change', onChange)
  }, [mode])

  return [mode, setThemeMode]
}

function setThemeMode(mode: ThemeMode) {
  if (mode === 'system') localStorage.removeItem(storageKey)
  else localStorage.setItem(storageKey, mode)
  applyMode(storageKey)
  for (const listener of listeners) listener()
}

const listeners = new Set<() => void>()

function subscribe(listener: () => void) {
  // The storage event carries changes from other tabs.
  const onStorage = (event: StorageEvent) => {
    if (event.key !== storageKey) return
    applyMode(storageKey)
    listener()
  }
  listeners.add(listener)
  window.addEventListener('storage', onStorage)
  return () => {
    listeners.delete(listener)
    window.removeEventListener('storage', onStorage)
  }
}

function readMode(): ThemeMode {
  const saved = localStorage.getItem(storageKey)
  return saved === 'light' || saved === 'dark' ? saved : 'system'
}

function applyMode(key: string) {
  const saved = localStorage.getItem(key)
  const dark = saved === 'dark' || (saved !== 'light' && matchMedia('(prefers-color-scheme: dark)').matches)
  document.documentElement.classList.toggle('dark', dark)
}
