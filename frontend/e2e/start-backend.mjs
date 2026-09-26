import { spawn } from 'node:child_process'
import { mkdirSync, mkdtempSync } from 'node:fs'
import { resolve, join } from 'node:path'
import { randomBytes } from 'node:crypto'

const tools = resolve('../.tools')
mkdirSync(tools, { recursive: true })
const data = mkdtempSync(join(tools, 'e2e-data-'))
const command = process.env.LINKBOX_TEST_BINARY || 'go'
const args = process.env.LINKBOX_TEST_BINARY ? [] : ['run', './cmd/server']
const backend = spawn(command, args, {
  cwd: resolve('../backend'), stdio: 'inherit',
  env: {
    ...process.env, APP_URL: 'http://localhost:18773', LISTEN_ADDR: '127.0.0.1:18780',
    COOKIE_SECURE: 'false', TRUST_PROXY: 'false', ADMIN_USERNAME: 'admin',
    ADMIN_PASSWORD: 'e2e-local-test-password', SESSION_SECRET: randomBytes(32).toString('hex'),
    DATABASE_PATH: join(data, 'database', 'linkbox.db'), STORAGE_PATH: join(data, 'files'), CHUNK_PATH: join(data, 'chunks'),
    UPLOAD_CHUNK_SIZE: '33554432', STORAGE_RESERVED_BYTES: '0', LOG_LEVEL: 'warn',
  },
})
backend.on('exit', code => process.exit(code || 0))
for (const signal of ['SIGINT', 'SIGTERM']) process.on(signal, () => backend.kill(signal))
