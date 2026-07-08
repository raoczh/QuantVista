<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { useRoute } from 'vue-router'
import {
  NButton,
  NTag,
  NTable,
  NGrid,
  NGi,
  NInputNumber,
  NSelect,
  NRadioGroup,
  NRadioButton,
  NCheckboxGroup,
  NCheckbox,
  NSwitch,
  NSpin,
  NEmpty,
  useMessage,
  type SelectOption,
} from 'naive-ui'
import {
  runBacktest,
  backtestRecommendations,
  HOLD_STATUS_LABEL,
  type BacktestResult,
  type BacktestHoldStat,
  type BacktestTrade,
  type BatchBacktestResult,
} from '@/api/backtest'
import { getScreenerStrategies, PERIOD_LABEL, type StrategiesView } from '@/api/screener'
import { listRecommendations, type RecommendationBatch } from '@/api/recommendation'
import { useUi } from '@/composables/useUi'
import { useIsMobile } from '@/composables/useIsMobile'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import ChangeTag from '@/components/ChangeTag.vue'

const message = useMessage()
const route = useRoute()
const { vars, pctColor } = useUi()
const { isMobile } = useIsMobile()
const styleVars = computed(() => ({ '--qv-divider': vars.value.dividerColor }))

const tab = ref<'strategy' | 'recs'>('strategy')

// ---------- 策略回测 ----------

const strategies = ref<StrategiesView | null>(null)
const strategyValue = ref<string>('') // `b:{key}` 内置 / `c:{id}` 自定义
const lookbackDays = ref(60)
const signalCount = ref(8)
const holdDays = ref<number[]>([5, 10, 20])
const perStockCap = ref(20000)
const includeST = ref(false)
const running = ref(false)
const result = ref<BacktestResult | null>(null)

const strategyOptions = computed<SelectOption[]>(() => {
  const s = strategies.value
  if (!s) return []
  const groups: SelectOption[] = []
  const byPeriod: Record<string, SelectOption[]> = {}
  for (const b of s.builtin) {
    ;(byPeriod[b.period] = byPeriod[b.period] ?? []).push({ label: b.name, value: `b:${b.key}` })
  }
  for (const p of ['short', 'swing', 'mid']) {
    if (byPeriod[p]?.length) {
      groups.push({ type: 'group', label: `内置 · ${PERIOD_LABEL[p] ?? p}`, key: p, children: byPeriod[p] })
    }
  }
  const custom = (s.custom ?? []).map((c) => ({ label: c.name, value: `c:${c.id}` }))
  if (custom.length) groups.push({ type: 'group', label: '我的策略', key: 'custom', children: custom })
  return groups
})

async function loadStrategies() {
  try {
    strategies.value = await getScreenerStrategies()
    // 深链 ?strategy_key=xxx（选股页「回测」按钮跳转）。
    const key = route.query.strategy_key as string
    if (key && strategies.value.builtin.some((b) => b.key === key)) {
      strategyValue.value = `b:${key}`
      run()
    } else if (!strategyValue.value && strategies.value.builtin.length) {
      strategyValue.value = `b:${strategies.value.builtin[0].key}`
    }
  } catch (e) {
    message.error((e as Error).message)
  }
}

async function run() {
  if (!strategyValue.value) {
    message.warning('请先选择策略')
    return
  }
  if (!holdDays.value.length) {
    message.warning('请至少选择一个持有期')
    return
  }
  running.value = true
  try {
    const [kind, id] = [strategyValue.value.slice(0, 1), strategyValue.value.slice(2)]
    result.value = await runBacktest({
      strategy_key: kind === 'b' ? id : undefined,
      strategy_id: kind === 'c' ? Number(id) : undefined,
      lookback_days: lookbackDays.value,
      signal_count: signalCount.value,
      hold_days: [...holdDays.value].sort((a, b) => a - b),
      per_stock_cap: perStockCap.value,
      include_st: includeST.value,
    })
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    running.value = false
  }
}

function skipSummary(st: BacktestHoldStat): string {
  const parts: string[] = []
  if (st.skip_limit_up) parts.push(`一字板放弃 ${st.skip_limit_up}`)
  if (st.skip_cash) parts.push(`拨款不足 ${st.skip_cash}`)
  if (st.skip_suspend) parts.push(`停牌跳过 ${st.skip_suspend}`)
  if (st.pending) parts.push(`未走完 ${st.pending}`)
  if (st.deferred) parts.push(`跌停顺延 ${st.deferred} 次`)
  if (st.forced) parts.push(`强平 ${st.forced}`)
  return parts.length ? parts.join(' · ') : '无跳过'
}

function fmtPct(v: number | undefined): string {
  if (v === undefined || v === null) return '—'
  return `${v > 0 ? '+' : ''}${v.toFixed(2)}%`
}

function tradeAlpha(tr: BacktestTrade): string {
  return tr.alpha_pct === undefined || tr.alpha_pct === null ? '—' : fmtPct(tr.alpha_pct)
}

// ---------- 推荐批次回验 ----------

const batches = ref<RecommendationBatch[]>([])
const batchValue = ref<number>(0) // 0=近 90 天全部
const recRunning = ref(false)
const recResult = ref<BatchBacktestResult | null>(null)

const batchOptions = computed<SelectOption[]>(() => [
  { label: '近 90 天全部成功批次', value: 0 },
  ...batches.value
    .filter((b) => b.status === 'success')
    .map((b) => ({
      label: `#${b.id} ${b.title || b.strategy}（${(b.created_at || '').slice(0, 10)}）`,
      value: b.id,
    })),
])

async function loadBatches() {
  try {
    batches.value = await listRecommendations(undefined, 50)
  } catch {
    /* 批次列表拉不到不阻断（下拉仅剩「全部」选项） */
  }
}

async function runRecs() {
  recRunning.value = true
  try {
    recResult.value = await backtestRecommendations(batchValue.value)
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    recRunning.value = false
  }
}

function histMax(hist: { count: number }[] | null | undefined): number {
  return Math.max(1, ...(hist ?? []).map((b) => b.count))
}

const recHold = ref('20') // 直方图/明细展示的持有期
const recHoldStat = computed(() => recResult.value?.stats.find((s) => String(s.hold_days) === recHold.value))

onMounted(() => {
  loadStrategies()
  loadBatches()
})
</script>

<template>
  <PageContainer
    title="回测时光机"
    subtitle="策略与 AI 推荐的历史可验证：次日开盘买入 · A 股真实约束 · 上证基准对照"
    :style="styleVars"
  >
    <n-radio-group v-model:value="tab" size="small" style="margin-bottom: 16px">
      <n-radio-button value="strategy">策略回测</n-radio-button>
      <n-radio-button value="recs">推荐批次回验</n-radio-button>
    </n-radio-group>

    <!-- ================= 策略回测 ================= -->
    <div v-if="tab === 'strategy'" class="bt-layout">
      <div class="bt-side">
        <SectionCard title="回测参数">
          <div class="form-col">
            <div class="form-row">
              <span class="form-label">选股策略</span>
              <n-select v-model:value="strategyValue" :options="strategyOptions" filterable placeholder="选择策略" />
            </div>
            <div class="form-row">
              <span class="form-label">回看窗口（交易日）</span>
              <n-input-number v-model:value="lookbackDays" :min="10" :max="180" :step="10" style="width: 100%" />
            </div>
            <div class="form-row">
              <span class="form-label">采样信号日数</span>
              <n-input-number v-model:value="signalCount" :min="1" :max="16" style="width: 100%" />
            </div>
            <div class="form-row">
              <span class="form-label">持有期（交易日）</span>
              <n-checkbox-group v-model:value="holdDays">
                <n-checkbox :value="5" label="5 日" />
                <n-checkbox :value="10" label="10 日" />
                <n-checkbox :value="20" label="20 日" />
              </n-checkbox-group>
            </div>
            <div class="form-row">
              <span class="form-label">每标的拨款（元）</span>
              <n-input-number v-model:value="perStockCap" :min="5000" :max="1000000" :step="5000" style="width: 100%" />
            </div>
            <div class="form-row form-row-inline">
              <span class="form-label">包含 ST/风险警示</span>
              <n-switch v-model:value="includeST" size="small" />
            </div>
            <n-button type="primary" block :loading="running" @click="run">开始回测</n-button>
            <div class="hint">
              回测为数秒级计算且全局互斥；信号日按窗口等距采样，每信号日按成交额取前 200 只命中进入模拟。
            </div>
          </div>
        </SectionCard>
      </div>

      <div class="bt-main">
        <n-spin :show="running">
          <template v-if="result">
            <SectionCard :title="`回测结果 · ${result.strategy}`">
              <div class="meta-line">
                数据截至 {{ result.trade_date }} · 信号日 {{ result.signal_dates.length }} 个 ·
                宇宙 {{ result.universe }} 只（ST 跳过 {{ result.st_skipped }}，复权可疑剔除 {{ result.adjust_suspect }}）·
                耗时 {{ (result.elapsed_ms / 1000).toFixed(1) }}s
              </div>
              <div v-if="result.conditions?.length" class="cond-line">
                <n-tag v-for="(c, i) in result.conditions" :key="i" size="small" :bordered="false" style="margin: 0 6px 6px 0">
                  {{ c }}
                </n-tag>
              </div>

              <div v-for="st in result.stats" :key="st.hold_days" class="hold-block">
                <div class="hold-title">持有 {{ st.hold_days }} 个交易日（成交 {{ st.trades }} 笔）</div>
                <n-grid :cols="isMobile ? 2 : 4" :x-gap="12" :y-gap="12">
                  <n-gi>
                    <div class="stat-box">
                      <div class="stat-label">胜率</div>
                      <div class="stat-value qv-figure">{{ st.trades ? `${st.win_rate.toFixed(1)}%` : '—' }}</div>
                    </div>
                  </n-gi>
                  <n-gi>
                    <div class="stat-box">
                      <div class="stat-label">平均收益</div>
                      <div class="stat-value qv-figure" :style="{ color: st.trades ? pctColor(st.avg_return_pct) : undefined }">
                        {{ st.trades ? fmtPct(st.avg_return_pct) : '—' }}
                      </div>
                    </div>
                  </n-gi>
                  <n-gi>
                    <div class="stat-box">
                      <div class="stat-label">收益中位数</div>
                      <div class="stat-value qv-figure" :style="{ color: st.trades ? pctColor(st.median_return_pct) : undefined }">
                        {{ st.trades ? fmtPct(st.median_return_pct) : '—' }}
                      </div>
                    </div>
                  </n-gi>
                  <n-gi>
                    <div class="stat-box">
                      <div class="stat-label">平均超额(α)·基准 {{ st.alpha_sample ? fmtPct(st.bench_avg_pct) : '—' }}</div>
                      <div class="stat-value qv-figure" :style="{ color: st.alpha_sample ? pctColor(st.avg_alpha_pct) : undefined }">
                        {{ st.alpha_sample ? fmtPct(st.avg_alpha_pct) : '—' }}
                      </div>
                    </div>
                  </n-gi>
                </n-grid>
                <div class="skip-line">
                  最好 <span :style="{ color: pctColor(st.best_pct) }" class="qv-tnum">{{ st.trades ? fmtPct(st.best_pct) : '—' }}</span>
                  · 最差 <span :style="{ color: pctColor(st.worst_pct) }" class="qv-tnum">{{ st.trades ? fmtPct(st.worst_pct) : '—' }}</span>
                  · {{ skipSummary(st) }}
                </div>
              </div>
            </SectionCard>

            <SectionCard title="逐信号日">
              <div class="qv-scroll-x">
                <n-table size="small" :single-line="false">
                  <thead>
                    <tr>
                      <th>信号日</th>
                      <th>命中</th>
                      <th>进入模拟</th>
                      <th v-for="st in result.stats" :key="st.hold_days">{{ st.hold_days }}日均收</th>
                    </tr>
                  </thead>
                  <tbody>
                    <tr v-for="d in result.days" :key="d.date">
                      <td class="qv-tnum">{{ d.date }}</td>
                      <td class="qv-tnum">{{ d.matched }}</td>
                      <td class="qv-tnum">{{ d.taken }}</td>
                      <td v-for="st in result.stats" :key="st.hold_days" class="qv-tnum">
                        <span
                          v-if="d.traded_by_hold[String(st.hold_days)]"
                          :style="{ color: pctColor(d.avg_returns[String(st.hold_days)] ?? 0) }"
                        >
                          {{ fmtPct(d.avg_returns[String(st.hold_days)] ?? 0) }}
                        </span>
                        <span v-else>—</span>
                      </td>
                    </tr>
                  </tbody>
                </n-table>
              </div>
            </SectionCard>

            <SectionCard v-for="st in result.stats" :key="`s-${st.hold_days}`" :title="`持有 ${st.hold_days} 日 · 最好/最差样本`">
              <n-empty v-if="!st.best_trades?.length" description="无成交样本" />
              <div v-else class="qv-scroll-x">
                <n-table size="small" :single-line="false">
                  <thead>
                    <tr>
                      <th>标的</th>
                      <th>信号日</th>
                      <th>买入</th>
                      <th>卖出</th>
                      <th>收益</th>
                      <th>α</th>
                      <th>备注</th>
                    </tr>
                  </thead>
                  <tbody>
                    <tr v-for="tr in [...(st.best_trades ?? []), ...(st.worst_trades ?? [])]" :key="`${tr.symbol}-${tr.signal_date}-${tr.return_pct}`">
                      <td>{{ tr.name || tr.symbol }} <span class="dim qv-tnum">{{ tr.symbol }}</span></td>
                      <td class="qv-tnum">{{ tr.signal_date }}</td>
                      <td class="qv-tnum">{{ tr.buy_date }} @ {{ tr.buy_price.toFixed(2) }}</td>
                      <td class="qv-tnum">{{ tr.sell_date }} @ {{ tr.sell_price.toFixed(2) }}</td>
                      <td><ChangeTag :value="tr.return_pct" /></td>
                      <td class="qv-tnum">{{ tradeAlpha(tr) }}</td>
                      <td class="dim">
                        <template v-if="tr.deferred">跌停顺延{{ tr.deferred }}次</template>
                        <template v-if="tr.forced">（末根强平）</template>
                      </td>
                    </tr>
                  </tbody>
                </n-table>
              </div>
            </SectionCard>

            <SectionCard v-if="result.notes?.length" title="口径与披露">
              <ul class="notes">
                <li v-for="(n, i) in result.notes" :key="i">{{ n }}</li>
              </ul>
            </SectionCard>
          </template>
          <SectionCard v-else title="回测结果">
            <n-empty description="选择策略并点击「开始回测」——历史信号日按当日因子选股、次日开盘买入，统计持有 5/10/20 日的真实表现" />
          </SectionCard>
        </n-spin>
      </div>
    </div>

    <!-- ================= 推荐批次回验 ================= -->
    <div v-else class="bt-layout">
      <div class="bt-side">
        <SectionCard title="回验对象">
          <div class="form-col">
            <div class="form-row">
              <span class="form-label">推荐批次</span>
              <n-select v-model:value="batchValue" :options="batchOptions" />
            </div>
            <n-button type="primary" block :loading="recRunning" @click="runRecs">开始回验</n-button>
            <div class="hint">
              把历史推荐的标的按「推荐日次日开盘买入、持有 5/10/20 日」重演，输出相对上证的超额收益（alpha）分布——
              与推荐追踪的前向视角互补。
            </div>
          </div>
        </SectionCard>
      </div>

      <div class="bt-main">
        <n-spin :show="recRunning">
          <template v-if="recResult">
            <SectionCard :title="`回验结果 · ${recResult.batches} 个批次 / ${recResult.picks} 条推荐`">
              <n-grid :cols="isMobile ? 1 : 3" :x-gap="12" :y-gap="12">
                <n-gi v-for="st in recResult.stats" :key="st.hold_days">
                  <div class="rec-hold-card">
                    <div class="hold-title">持有 {{ st.hold_days }} 日</div>
                    <div class="rec-hold-line">
                      成交 {{ st.trades }} · 胜率
                      <span class="qv-tnum">{{ st.trades ? `${st.win_rate.toFixed(1)}%` : '—' }}</span>
                    </div>
                    <div class="rec-hold-line">
                      均收
                      <span class="qv-tnum" :style="{ color: st.trades ? pctColor(st.avg_return_pct) : undefined }">
                        {{ st.trades ? fmtPct(st.avg_return_pct) : '—' }}
                      </span>
                      · 均α
                      <span class="qv-tnum" :style="{ color: st.alpha_sample ? pctColor(st.avg_alpha_pct) : undefined }">
                        {{ st.alpha_sample ? fmtPct(st.avg_alpha_pct) : '—' }}
                      </span>
                    </div>
                    <div class="rec-hold-line dim">
                      未走完 {{ st.pending }} · 跳过 {{ st.skipped }} · 无数据 {{ st.no_data }}
                    </div>
                  </div>
                </n-gi>
              </n-grid>
            </SectionCard>

            <SectionCard title="alpha 分布">
              <template #header-extra>
                <n-radio-group v-model:value="recHold" size="small">
                  <n-radio-button v-for="st in recResult.stats" :key="st.hold_days" :value="String(st.hold_days)">
                    {{ st.hold_days }}日
                  </n-radio-button>
                </n-radio-group>
              </template>
              <n-empty v-if="!recHoldStat || !recHoldStat.alpha_sample" description="该持有期暂无 alpha 样本（持有期未走完或基准缺失）" />
              <div v-else class="hist">
                <div v-for="b in recHoldStat.alpha_hist" :key="b.label" class="hist-row">
                  <span class="hist-label qv-tnum">{{ b.label }}</span>
                  <div class="hist-bar-wrap">
                    <div
                      class="hist-bar"
                      :style="{
                        width: `${(b.count / histMax(recHoldStat.alpha_hist)) * 100}%`,
                        background: vars.primaryColor,
                      }"
                    />
                  </div>
                  <span class="hist-count qv-tnum">{{ b.count }}</span>
                </div>
              </div>
            </SectionCard>

            <SectionCard title="逐条明细">
              <n-empty v-if="!recResult.rows?.length" description="无明细" />
              <div v-else class="qv-scroll-x">
                <n-table size="small" :single-line="false">
                  <thead>
                    <tr>
                      <th>批次</th>
                      <th>标的</th>
                      <th>信号日</th>
                      <th v-for="st in recResult.stats" :key="st.hold_days">{{ st.hold_days }}日收益 / α</th>
                    </tr>
                  </thead>
                  <tbody>
                    <tr v-for="(r, i) in recResult.rows" :key="i">
                      <td class="dim">#{{ r.batch_id }} {{ r.batch_title }}</td>
                      <td>{{ r.name || r.symbol }} <span class="dim qv-tnum">{{ r.symbol }}</span></td>
                      <td class="qv-tnum">{{ r.signal_date || '—' }}</td>
                      <td v-for="st in recResult.stats" :key="st.hold_days" class="qv-tnum">
                        <template v-if="r.holds[String(st.hold_days)]?.status === 'traded'">
                          <span :style="{ color: pctColor(r.holds[String(st.hold_days)].return_pct) }">
                            {{ fmtPct(r.holds[String(st.hold_days)].return_pct) }}
                          </span>
                          <span class="dim">
                            /
                            {{
                              r.holds[String(st.hold_days)].alpha_pct !== undefined
                                ? fmtPct(r.holds[String(st.hold_days)].alpha_pct)
                                : '—'
                            }}
                          </span>
                        </template>
                        <span v-else class="dim">{{ HOLD_STATUS_LABEL[r.holds[String(st.hold_days)]?.status ?? ''] ?? '—' }}</span>
                      </td>
                    </tr>
                  </tbody>
                </n-table>
              </div>
            </SectionCard>

            <SectionCard v-if="recResult.notes?.length" title="口径与披露">
              <ul class="notes">
                <li v-for="(n, i) in recResult.notes" :key="i">{{ n }}</li>
              </ul>
            </SectionCard>
          </template>
          <SectionCard v-else title="回验结果">
            <n-empty description="选择批次（或近 90 天全部）并点击「开始回验」" />
          </SectionCard>
        </n-spin>
      </div>
    </div>
  </PageContainer>
</template>

<style scoped>
.bt-layout {
  display: flex;
  gap: 16px;
  align-items: flex-start;
}
.bt-side {
  width: 300px;
  flex-shrink: 0;
  position: sticky;
  top: 76px;
}
.bt-main {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.form-col {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.form-row {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.form-row-inline {
  flex-direction: row;
  align-items: center;
  justify-content: space-between;
}
.form-label {
  font-size: 13px;
  opacity: 0.75;
}
.hint {
  font-size: 12px;
  opacity: 0.6;
  line-height: 1.6;
}
.meta-line {
  font-size: 13px;
  opacity: 0.75;
  margin-bottom: 10px;
}
.cond-line {
  margin-bottom: 8px;
}
.hold-block {
  padding: 12px 0;
  border-top: 1px dashed var(--qv-divider);
}
.hold-block:first-of-type {
  border-top: none;
}
.hold-title {
  font-weight: 600;
  margin-bottom: 10px;
}
.stat-box {
  border: 1px solid var(--qv-divider);
  border-radius: 8px;
  padding: 10px 12px;
}
.stat-label {
  font-size: 12px;
  opacity: 0.65;
  margin-bottom: 6px;
}
.stat-value {
  font-size: 20px;
  font-weight: 700;
  line-height: 1.1;
}
.skip-line {
  margin-top: 10px;
  font-size: 12.5px;
  opacity: 0.75;
}
.notes {
  margin: 0;
  padding-left: 18px;
  font-size: 12.5px;
  opacity: 0.7;
  line-height: 1.9;
}
.dim {
  opacity: 0.6;
  font-size: 12px;
}
.rec-hold-card {
  border: 1px solid var(--qv-divider);
  border-radius: 8px;
  padding: 12px 14px;
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.rec-hold-line {
  font-size: 13px;
}
.hist {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.hist-row {
  display: flex;
  align-items: center;
  gap: 10px;
}
.hist-label {
  width: 92px;
  text-align: right;
  font-size: 12.5px;
  opacity: 0.75;
  flex-shrink: 0;
}
.hist-bar-wrap {
  flex: 1;
  min-width: 0;
}
.hist-bar {
  height: 16px;
  border-radius: 4px;
  min-width: 2px;
  opacity: 0.85;
  transition: width 0.3s;
}
.hist-count {
  width: 36px;
  font-size: 12.5px;
  flex-shrink: 0;
}

@media (max-width: 768px) {
  .bt-layout {
    flex-direction: column;
  }
  .bt-side {
    width: 100%;
    position: static;
  }
}
</style>
