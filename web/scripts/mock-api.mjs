// Development stand-in for the Go API with the sample data of the designs.
// It answers only the endpoints of the app shell. Run it with the dev server:
//   node scripts/mock-api.mjs 8499 & RUNPLANE_API=http://localhost:8499 pnpm dev
// MOCK_NEEDS_SETUP=1 answers as a fresh install, so the app sends you to /setup.
// MOCK_SIGNED_OUT=1 answers without a session, so the app sends you to /login.
// POST /mock/events {"event":"job.state","data":{...}} sends an SSE event to open pages;
// a job.state event for a job that is no longer running drops it from the running list.
// POST /mock/drop ends the open event streams, as a server restart does.
import { createServer } from 'node:http'

const port = Number(process.argv[2] ?? 8499)
const needsSetup = process.env.MOCK_NEEDS_SETUP === '1'
const signedOut = needsSetup || process.env.MOCK_SIGNED_OUT === '1'

const user = { id: '01jbx7m2k8q4r6t9v3w5y7z1a2', email: 'ada@kestrel.dev', name: 'Ada Okafor', role: 'admin' }
const status = {
  needs_setup: needsSetup,
  version: '0.4.2',
  mode: 'container',
  instance_name: 'runplane',
  github_connected: true,
  has_jobs: true,
}
const github = {
  app: { app_id: 1048213, slug: 'runplane-kestrel', name: 'runplane-kestrel', owner_login: 'kestrel-hq', html_url: 'https://github.com/apps/runplane-kestrel' },
  installations: [
    {
      id: 52011873,
      account_login: 'kestrel-hq',
      account_type: 'Organization',
      avatar_url: '',
      repository_selection: 'all',
      repository_count: 12,
      suspended_at: null,
    },
  ],
}
const running = ['test (node 22)', 'e2e · chromium', 'build-image', 'lint', 'typecheck', 'e2e · webkit', 'deploy-staging'].map(
  (name, i) => ({
    id: `job${i}`,
    repository: 'kestrel-hq/api',
    workflow_name: 'ci',
    job_name: name,
    state: 'running',
    result: null,
    queued_at: new Date(Date.now() - (i + 1) * 60_000).toISOString(),
  }),
)

const routes = {
  'GET /api/status': () => [200, status],
  'GET /api/session': () => (signedOut ? [401, problem(401, 'no session')] : [200, user]),
  'POST /api/session': () => [200, user],
  'DELETE /api/session': () => [204],
  'PATCH /api/me': () => [200, user],
  'GET /api/github': () => [200, github],
  'GET /api/jobs': () => [200, { items: running, next_cursor: null }],
}

const streams = new Set()

createServer(async (req, res) => {
  const path = new URL(req.url, 'http://localhost').pathname
  if (req.method === 'GET' && path === '/api/events') {
    res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' })
    // EventSource reports "open" only when the headers arrive.
    res.flushHeaders()
    streams.add(res)
    const ping = setInterval(() => res.write(': ping\n\n'), 15_000)
    req.on('close', () => {
      clearInterval(ping)
      streams.delete(res)
    })
    return
  }
  if (req.method === 'POST' && path === '/mock/events') {
    let body = ''
    for await (const chunk of req) body += chunk
    const { event, data } = JSON.parse(body)
    if (event === 'job.state' && data.state !== 'running') {
      const index = running.findIndex((job) => job.id === data.id)
      if (index >= 0) running.splice(index, 1)
    }
    for (const stream of streams) stream.write(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`)
    res.writeHead(204).end()
    return
  }
  if (req.method === 'POST' && path === '/mock/drop') {
    for (const stream of streams) stream.end()
    res.writeHead(204).end()
    return
  }
  const route = routes[`${req.method} ${path}`]
  const [code, body] = route ? route() : [404, problem(404, `no mock for ${req.method} ${path}`)]
  res.writeHead(code, body ? { 'Content-Type': code < 400 ? 'application/json' : 'application/problem+json' } : {})
  res.end(body ? JSON.stringify(body) : undefined)
}).listen(port, () => console.log(`mock API on http://localhost:${port}`))

function problem(status, detail) {
  return { title: status === 401 ? 'Unauthorized' : 'Not Found', status, detail }
}
