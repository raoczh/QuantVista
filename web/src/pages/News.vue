<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { NAlert, NButton, NEmpty, NInput, NRadioButton, NRadioGroup, NSpin, NTag } from 'naive-ui'
import { getNews, newsSourceLabel, relatedStocks, sentimentTag, type NewsItem, type NewsRelatedStock } from '@/api/news'
import { useUi, withAlpha } from '@/composables/useUi'
import { useAutoRefresh } from '@/composables/useAutoRefresh'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import StockIdentity from '@/components/StockIdentity.vue'

const route = useRoute()
const { vars, isDark, pctColor } = useUi()

// 情绪标签（N2）：利好/利空才渲染，颜色随涨跌色主题。
function sentiView(n: NewsItem): { text: string; color: string } | null {
  const t = sentimentTag(n)
  return t ? { text: t.text, color: pctColor(t.dir) } : null
}

// ---------- 筛选 ----------
const sourceOptions = [
  { label: '全部来源', value: '' },
  { label: '财联社电报', value: 'cls' },
  { label: '东方财富', value: 'eastmoney' },
]
const source = ref('')
// 代码筛选：输入中与已生效分离，回车/清空才触发查询，避免打一半就打接口。
const symbolInput = ref('')
const symbol = ref('')

// ---------- 数据 ----------
const PAGE_SIZE = 60
const MAX_LIMIT = 200 // 后端 ListNews 上限
const limit = ref(PAGE_SIZE)
const items = ref<NewsItem[]>([])
const loading = ref(false)
const loadingMore = ref(false)
const loadError = ref('')

// 筛选切换/自动刷新竞态守卫：快速切来源或改代码时旧响应不覆盖新结果。
// 分页上限只在相应请求成功后推进，失效请求不会回滚新筛选的状态。
let refreshSeq = 0
let filterSeq = 0
async function refresh(silent = false, requestedLimit = limit.value) {
  const mySeq = ++refreshSeq
  if (!silent) loading.value = true
  try {
    const data = await getNews({
      symbol: symbol.value || undefined,
      source: source.value || undefined,
      limit: requestedLimit,
    })
    if (mySeq !== refreshSeq) return
    items.value = data
    limit.value = requestedLimit
    loadError.value = ''
  } catch (e) {
    if (mySeq !== refreshSeq) return
    loadError.value = e instanceof Error ? e.message : '新闻查询失败'
  } finally {
    if (mySeq === refreshSeq) loading.value = false
  }
}

function resetFeed() {
  filterSeq++
  limit.value = PAGE_SIZE
  items.value = []
  loadError.value = ''
  loadingMore.value = false
  void refresh()
}

function applyFilter() {
  symbol.value = symbolInput.value.trim()
  resetFeed()
}
function onSourceChange(v: string) {
  source.value = v
  resetFeed()
}
function clearSymbol() {
  symbolInput.value = ''
  if (symbol.value) {
    symbol.value = ''
    resetFeed()
  }
}

// 返回条数打满当前 limit 才可能还有下一页；到后端上限为止。
const hasMore = computed(() => items.value.length >= limit.value && limit.value < MAX_LIMIT)
async function loadMore() {
  if (loading.value || loadingMore.value || !hasMore.value) return
  loadingMore.value = true
  const owner = filterSeq
  try {
    await refresh(true, Math.min(limit.value + PAGE_SIZE, MAX_LIMIT))
  } finally {
    if (owner === filterSeq) loadingMore.value = false
  }
}

watch(() => route.query.symbol, value => {
  // 支持 /news?symbol= 深链（个股详情「更多」入口）。
  const q = String((Array.isArray(value) ? value[0] : value) || '').trim()
  symbolInput.value = q
  symbol.value = q
  resetFeed()
}, { immediate: true })
onBeforeUnmount(() => { refreshSeq++; filterSeq++ })
useAutoRefresh(() => {
  if (!loading.value && !loadingMore.value) return refresh(true)
})

// ---------- 展示 ----------
// 关联标的在 groups 里一次算好：模板里要用 3 次（截断渲染 + 溢出计数），
// 逐次调用会对每条快讯重复解析一遍（老响应还要 JSON.parse）。
const MAX_RELATED = 4
interface FeedEntry {
  news: NewsItem
  stocks: NewsRelatedStock[]
  moreCount: number
}
interface FeedGroup {
  key: string
  label: string
  items: FeedEntry[]
}

function dateLabel(d: Date): string {
  const now = new Date()
  const today = new Date(now.getFullYear(), now.getMonth(), now.getDate())
  const day = new Date(d.getFullYear(), d.getMonth(), d.getDate())
  const diffDays = Math.round((today.getTime() - day.getTime()) / 86_400_000)
  const base = `${d.getMonth() + 1} 月 ${d.getDate()} 日`
  if (diffDays === 0) return `今天 · ${base}`
  if (diffDays === 1) return `昨天 · ${base}`
  const week = ['日', '一', '二', '三', '四', '五', '六'][d.getDay()]
  return `${base} · 周${week}`
}

// 按发布日期分组（后端已按 publish_time 倒序返回）。
const groups = computed<FeedGroup[]>(() => {
  const out: FeedGroup[] = []
  let cur: FeedGroup | null = null
  for (const n of items.value) {
    const d = new Date(n.publish_time)
    const key = `${d.getFullYear()}-${d.getMonth() + 1}-${d.getDate()}`
    if (!cur || cur.key !== key) {
      cur = { key, label: dateLabel(d), items: [] }
      out.push(cur)
    }
    const all = relatedStocks(n)
    cur.items.push({
      news: n,
      stocks: all.slice(0, MAX_RELATED),
      moreCount: Math.max(0, all.length - MAX_RELATED),
    })
  }
  return out
})

function fmtTime(t: string): string {
  const d = new Date(t)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${p(d.getHours())}:${p(d.getMinutes())}`
}

const latestPublished = computed(() => {
  const value = items.value[0]?.publish_time
  return value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '未知'
})

// 电报类 title 常是正文截断，summary 与 title 同头时不重复展示。
function showSummary(n: NewsItem): boolean {
  const s = (n.summary || '').trim()
  if (!s) return false
  const t = (n.title || '').trim()
  return s !== t && !t.startsWith(s) && !s.startsWith(t)
}

const feedVars = computed(() => ({
  '--nf-line': vars.value.dividerColor,
  '--nf-imp': vars.value.errorColor,
  '--nf-imp-bg': withAlpha(vars.value.errorColor, isDark.value ? 0.14 : 0.08),
  '--nf-tag-bg': isDark.value ? 'rgba(255, 255, 255, 0.08)' : 'rgba(128, 128, 128, 0.1)',
}))
</script>

<template>
  <PageContainer title="市场快讯" subtitle="财联社电报 · 东财 7×24 快讯 · 个股新闻，盘中自动更新">
    <template #actions>
      <n-button size="small" secondary :loading="loading" :disabled="loadingMore" @click="refresh()">刷新</n-button>
    </template>

    <SectionCard :hoverable="false">
      <p class="feed-status">当前 {{ items.length }} 条 · 最新发布时间 {{ latestPublished }} · 来源与关联标的均按原始快讯记录展示</p>
      <!-- 筛选行 -->
      <div class="filters">
        <n-radio-group :value="source" size="small" @update:value="onSourceChange">
          <n-radio-button v-for="o in sourceOptions" :key="o.value" :value="o.value" :label="o.label" />
        </n-radio-group>
        <n-input
          v-model:value="symbolInput"
          size="small"
          clearable
          placeholder="按个股代码筛选，如 600519，回车生效"
          class="sym-input"
          @keyup.enter="applyFilter"
          @clear="clearSymbol"
        />
        <n-tag v-if="symbol" size="small" round closable @close="clearSymbol">
          个股 <span class="qv-tnum">{{ symbol }}</span>
        </n-tag>
      </div>

      <n-alert v-if="loadError" type="warning" class="feed-error" :title="items.length ? '刷新失败，以下为上次成功加载的快讯' : '加载失败'">
        {{ loadError }}
        <div class="feed-retry"><n-button size="small" :loading="loading" @click="refresh()">重试</n-button></div>
      </n-alert>
      <n-spin :show="loading">
        <n-empty
          v-if="!items.length && !loading && !loadError"
          description="当前筛选下暂无快讯，采集任务会定期更新"
          class="feed-empty"
        />

        <div v-if="items.length" class="feed" :style="feedVars">
          <template v-for="g in groups" :key="g.key">
            <div class="feed-date">
              <span class="fd-pill">{{ g.label }}</span>
            </div>
            <article
              v-for="e in g.items"
              :key="e.news.id"
              class="feed-item"
              :class="{ 'is-important': e.news.important_mark }"
            >
              <span class="fi-time qv-tnum">{{ fmtTime(e.news.publish_time) }}</span>
              <div class="fi-body">
                <div class="fi-title-row">
                  <span v-if="e.news.important_mark" class="fi-imp-mark">重要</span>
                  <a
                    v-if="e.news.url"
                    :href="e.news.url"
                    target="_blank"
                    rel="noopener noreferrer"
                    class="fi-title"
                  >{{ e.news.title }}</a>
                  <span v-else class="fi-title">{{ e.news.title }}</span>
                </div>
                <p v-if="showSummary(e.news)" class="fi-summary">{{ e.news.summary }}</p>
                <div class="fi-meta">
                  <span class="fi-src">{{ newsSourceLabel(e.news) }}</span>
                  <span
                    v-if="sentiView(e.news)"
                    class="fi-senti"
                    :style="{ color: sentiView(e.news)!.color, background: withAlpha(sentiView(e.news)!.color, isDark ? 0.16 : 0.1) }"
                  >{{ sentiView(e.news)!.text }}</span>
                  <StockIdentity
                    v-for="s in e.stocks"
                    :key="s.symbol"
                    :symbol="s.symbol"
                    :market="s.market"
                    :name="s.name"
                    density="compact"
                    clickable
                    name-optional
                  />
                  <span v-if="e.moreCount" class="fi-sym-more">
                    +{{ e.moreCount }}
                  </span>
                </div>
              </div>
            </article>
          </template>

          <div v-if="hasMore" class="feed-more">
            <n-button size="small" quaternary :loading="loadingMore" :disabled="loading" @click="loadMore">
              加载更多
            </n-button>
          </div>
          <div v-else-if="items.length >= MAX_LIMIT" class="feed-more feed-more-hint">
            最多展示最近 {{ MAX_LIMIT }} 条
          </div>
        </div>
      </n-spin>
    </SectionCard>
  </PageContainer>
</template>

<style scoped>
.filters {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
  margin-bottom: 14px;
}
.feed-status { margin: 0 0 12px; font-size: 12px; opacity: .62; }
.feed-error { margin-bottom: 14px; }
.feed-retry { margin-top: 8px; }
.sym-input {
  width: 260px;
  max-width: 100%;
}

.feed-empty {
  padding: 36px 0;
}

/* ---------- 时间线快讯流 ---------- */
.feed {
  display: flex;
  flex-direction: column;
}
.feed-date {
  padding: 6px 0 10px;
}
.feed-date:not(:first-child) {
  margin-top: 10px;
}
.fd-pill {
  display: inline-block;
  font-size: 12px;
  font-weight: 600;
  padding: 3px 12px;
  border-radius: 999px;
  background: var(--nf-tag-bg);
  opacity: 0.85;
}

.feed-item {
  display: flex;
  gap: 14px;
  padding: 10px 0 12px 4px;
  border-left: 2px solid var(--nf-line);
  margin-left: 22px;
  position: relative;
}
.feed-item::before {
  content: '';
  position: absolute;
  left: -5px;
  top: 16px;
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--nf-line);
}
.feed-item.is-important::before {
  background: var(--nf-imp);
  box-shadow: 0 0 0 3px var(--nf-imp-bg);
}
.fi-time {
  flex-shrink: 0;
  width: 44px;
  margin-left: 10px;
  font-size: 12px;
  opacity: 0.55;
  line-height: 22px;
}
.fi-body {
  flex: 1;
  min-width: 0;
}
.fi-title-row {
  display: flex;
  align-items: baseline;
  gap: 8px;
}
.fi-imp-mark {
  flex-shrink: 0;
  font-size: 11px;
  font-weight: 700;
  line-height: 18px;
  padding: 0 7px;
  border-radius: 999px;
  color: var(--nf-imp);
  background: var(--nf-imp-bg);
}
.fi-title {
  font-size: 14px;
  line-height: 22px;
  color: inherit;
  text-decoration: none;
  overflow-wrap: anywhere;
}
a.fi-title:hover {
  color: var(--qv-primary);
}
.feed-item.is-important .fi-title {
  font-weight: 600;
}
.fi-summary {
  margin: 4px 0 0;
  font-size: 13px;
  line-height: 1.6;
  opacity: 0.62;
  overflow-wrap: anywhere;
}
.fi-meta {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  margin-top: 6px;
}
.fi-src {
  font-size: 11px;
  padding: 1px 8px;
  border-radius: 999px;
  background: var(--nf-tag-bg);
  opacity: 0.75;
}
.fi-senti {
  font-size: 11px;
  font-weight: 600;
  padding: 1px 8px;
  border-radius: 999px;
}
.fi-sym-more {
  font-size: 11px;
  opacity: 0.5;
}

.feed-more {
  display: flex;
  justify-content: center;
  padding: 14px 0 2px;
}
.feed-more-hint {
  font-size: 12px;
  opacity: 0.5;
}

@media (max-width: 768px) {
  .sym-input {
    width: 100%;
  }
  .feed-item {
    margin-left: 2px;
    gap: 10px;
  }
  .fi-time {
    width: 38px;
    margin-left: 8px;
  }
}

@media (max-width: 480px) {
  .filters :deep(.n-radio-button) { padding: 0 8px; }
}
</style>
