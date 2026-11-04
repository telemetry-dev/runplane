import { defineConfig } from 'vitest/config'

// Separate from vite.config.ts: the TanStack Start plugin is not needed to test plain modules.
export default defineConfig({
  resolve: { tsconfigPaths: true },
  test: { include: ['src/**/*.test.ts'] },
})
