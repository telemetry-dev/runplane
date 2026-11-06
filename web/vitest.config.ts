import { defineConfig } from 'vitest/config'

// Separate from vite.config.ts: the TanStack Start plugin is not needed to test plain modules.
export default defineConfig({
  resolve: { tsconfigPaths: true },
  test: {
    include: ['src/**/*.test.ts'],
    // The worker processes start with these values, before a module makes its
    // formatters. Thus each machine gets the same time zone and locale. The
    // zone is not UTC and the locale is not en-US: a format that uses UTC in
    // place of the local time, or the browser locale in place of en-US, does
    // not pass.
    env: { TZ: 'Asia/Tokyo', LC_ALL: 'de_DE.UTF-8' },
  },
})
