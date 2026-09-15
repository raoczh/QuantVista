import { defineConfig } from '@playwright/test'

export default defineConfig({
  testDir: './e2e',
  outputDir: './.playwright-results',
  timeout: 25_000,
  expect: { timeout: 6_000 },
  fullyParallel: true,
  workers: 2,
  reporter: 'list',
  use: {
    baseURL: 'http://127.0.0.1:5188',
    channel: process.env.QV_BROWSER_CHANNEL || (process.platform === 'win32' ? 'msedge' : undefined),
    reducedMotion: 'reduce',
    screenshot: 'off',
    trace: 'off',
    serviceWorkers: 'block',
  },
  projects: [
    { name: 'desktop-light', use: { viewport: { width: 1440, height: 1000 }, colorScheme: 'light' } },
    { name: 'desktop-dark', use: { viewport: { width: 1440, height: 1000 }, colorScheme: 'dark' } },
    { name: 'tablet-light', use: { viewport: { width: 834, height: 1112 }, colorScheme: 'light' } },
    { name: 'tablet-dark', use: { viewport: { width: 834, height: 1112 }, colorScheme: 'dark' } },
    { name: 'mobile-light', use: { viewport: { width: 390, height: 844 }, colorScheme: 'light' } },
    { name: 'mobile-dark', use: { viewport: { width: 390, height: 844 }, colorScheme: 'dark' } },
  ],
  webServer: {
    command: 'npm run dev -- --host 127.0.0.1 --port 5188 --strictPort',
    url: 'http://127.0.0.1:5188',
    reuseExistingServer: !process.env.CI,
    timeout: 30_000,
  },
})
