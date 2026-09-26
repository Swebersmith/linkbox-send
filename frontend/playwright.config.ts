import { defineConfig, devices } from '@playwright/test'

export default defineConfig({
  testDir: './e2e',
  timeout: 120_000,
  workers: 1,
  fullyParallel: false,
  reporter: 'list',
  use: {
    baseURL: 'http://localhost:18773',
    trace: 'retain-on-failure',
    ...devices['Desktop Chrome'],
    launchOptions: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE } : {},
  },
  webServer: [
    { command: 'node e2e/start-backend.mjs', url: 'http://127.0.0.1:18780/health', reuseExistingServer: false, timeout: 120_000 },
    { command: 'npm run dev -- --host localhost --port 18773', env: { LINKBOX_API_TARGET: 'http://127.0.0.1:18780' }, url: 'http://localhost:18773', reuseExistingServer: false, timeout: 120_000 },
  ],
})
