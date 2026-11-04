import { createLucideIcon } from 'lucide-react'

// Glyphs of the designs that Lucide 1.x does not ship, mostly brand marks. They
// take the same props as the icons from lucide-react.

/** Lucide 1.x dropped brand icons; this is the Lucide GitHub glyph that the designs use. */
export const GithubIcon = createLucideIcon('github', [
  [
    'path',
    {
      d: 'M15 22v-4a4.8 4.8 0 0 0-1-3.5c3 0 6-2 6-5.5.08-1.25-.27-2.48-1-3.5.28-1.15.28-2.35 0-3.5 0 0-1 0-3 1.5-2.64-.5-5.36-.5-8 0C6 2 5 2 5 2c-.3 1.15-.3 2.35 0 3.5A5.403 5.403 0 0 0 4 9c0 3.5 3 5.5 6 5.5-.39.49-.68 1.05-.85 1.65-.17.6-.22 1.23-.15 1.85v4',
      key: 'tonef',
    },
  ],
  ['path', { d: 'M9 18c-4.51 2-5-2-7-2', key: '9comsn' }],
])

/** The earlier Lucide "cpu" glyph (8 pins) of the Runners navigation item. */
export const CpuIcon = createLucideIcon('cpu', [
  ['rect', { width: '16', height: '16', x: '4', y: '4', rx: '2', key: 'outer' }],
  ['rect', { width: '6', height: '6', x: '9', y: '9', rx: '1', key: 'inner' }],
  ['path', { d: 'M15 2v2', key: 'p1' }],
  ['path', { d: 'M15 20v2', key: 'p2' }],
  ['path', { d: 'M2 15h2', key: 'p3' }],
  ['path', { d: 'M2 9h2', key: 'p4' }],
  ['path', { d: 'M20 15h2', key: 'p5' }],
  ['path', { d: 'M20 9h2', key: 'p6' }],
  ['path', { d: 'M9 2v2', key: 'p7' }],
  ['path', { d: 'M9 20v2', key: 'p8' }],
])

/** The earlier Lucide "git-branch" glyph of the Branch cells: the curve runs from the right circle to the lower one. */
export const BranchIcon = createLucideIcon('git-branch', [
  ['path', { d: 'M6 3v12', key: 'stem' }],
  ['circle', { cx: '18', cy: '6', r: '3', key: 'branch' }],
  ['circle', { cx: '6', cy: '18', r: '3', key: 'base' }],
  ['path', { d: 'M18 9a9 9 0 0 1-9 9', key: 'curve' }],
])

/** The earlier Lucide Slack glyph of the notification channel designs. */
export const SlackIcon = createLucideIcon('slack', [
  ['rect', { width: '3', height: '8', x: '13', y: '2', rx: '1.5', key: 'a' }],
  ['path', { d: 'M19 8.5V10h1.5A1.5 1.5 0 1 0 19 8.5', key: 'b' }],
  ['rect', { width: '3', height: '8', x: '8', y: '14', rx: '1.5', key: 'c' }],
  ['path', { d: 'M5 15.5V14H3.5A1.5 1.5 0 1 0 5 15.5', key: 'd' }],
  ['rect', { width: '8', height: '3', x: '14', y: '13', rx: '1.5', key: 'e' }],
  ['path', { d: 'M15.5 19H14v1.5a1.5 1.5 0 1 0 1.5-1.5', key: 'f' }],
  ['rect', { width: '8', height: '3', x: '2', y: '8', rx: '1.5', key: 'g' }],
  ['path', { d: 'M8.5 5H10V3.5A1.5 1.5 0 1 0 8.5 5', key: 'h' }],
])

/** The filled Discord mark (Simple Icons); Lucide has no outline version. */
export const DiscordIcon = createLucideIcon('discord', [
  [
    'path',
    {
      d: 'M20.317 4.37a19.79 19.79 0 0 0-4.885-1.515.074.074 0 0 0-.079.037c-.21.375-.444.864-.608 1.25a18.27 18.27 0 0 0-5.487 0 12.64 12.64 0 0 0-.617-1.25.077.077 0 0 0-.079-.037A19.74 19.74 0 0 0 3.677 4.37a.07.07 0 0 0-.032.027C.533 9.046-.32 13.58.099 18.058a.082.082 0 0 0 .031.056 19.9 19.9 0 0 0 5.993 3.03.078.078 0 0 0 .084-.028c.462-.63.874-1.295 1.226-1.994a.076.076 0 0 0-.041-.106 13.1 13.1 0 0 1-1.872-.892.077.077 0 0 1-.008-.128c.126-.094.252-.192.372-.291a.074.074 0 0 1 .078-.01c3.928 1.793 8.18 1.793 12.062 0a.074.074 0 0 1 .078.01c.12.098.246.198.373.292a.077.077 0 0 1-.006.127 12.3 12.3 0 0 1-1.873.892.077.077 0 0 0-.041.107c.36.698.772 1.362 1.225 1.993a.076.076 0 0 0 .084.028 19.84 19.84 0 0 0 6.002-3.03.077.077 0 0 0 .032-.054c.5-5.177-.838-9.674-3.549-13.66a.061.061 0 0 0-.031-.03zM8.02 15.33c-1.182 0-2.157-1.085-2.157-2.419 0-1.333.956-2.419 2.157-2.419 1.21 0 2.176 1.096 2.157 2.42 0 1.333-.956 2.418-2.157 2.418zm7.975 0c-1.183 0-2.157-1.085-2.157-2.419 0-1.333.955-2.419 2.157-2.419 1.21 0 2.176 1.096 2.157 2.42 0 1.333-.946 2.418-2.157 2.418z',
      fill: 'currentColor',
      stroke: 'none',
      key: 'mark',
    },
  ],
])
