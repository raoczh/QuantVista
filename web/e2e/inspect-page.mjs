import { chromium } from '@playwright/test'
import { mockApi } from './mock-api.mjs'
import { richResponses } from './rich-responses.mjs'

// 供人工视觉复核使用。截图仅写到 stdout，不在仓库生成临时图片。
const browser = await chromium.launch({ channel: process.env.QV_BROWSER_CHANNEL || (process.platform === 'win32' ? 'msedge' : undefined), headless: true })
let result
try {
  const page = await browser.newPage({ viewport: { width: Number(process.env.QV_VIEW_WIDTH || 1440), height: Number(process.env.QV_VIEW_HEIGHT || 1000) }, reducedMotion: 'reduce' })
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  const path = process.env.QV_VIEW_PATH || '/'
  const api = await mockApi(page, { theme: process.env.QV_VIEW_THEME || 'light-blue', responses: process.env.QV_VIEW_RICH === '1' ? richResponses : undefined,
    anonymous: ['/login', '/setup', '/login/callback'].includes(path), setup: path === '/setup' })
  await page.goto(`http://127.0.0.1:5188${path}`, { waitUntil: 'networkidle' })
  await page.locator('main').waitFor({ timeout: 8000 }).catch(error => errors.push(error.message))
  result = { errors, unknown: [...new Set(api.unknown)], mutations: api.mutations, url: page.url(), body: (await page.locator('body').innerText()).slice(0, 2200), title: await page.title(), image: (await page.screenshot({ fullPage: false, type: 'jpeg', quality: 65 })).toString('base64') }
} finally {
  await browser.close()
}

console.log(JSON.stringify(result))
