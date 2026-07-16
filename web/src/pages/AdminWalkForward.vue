<script setup lang="ts">
import { h, onMounted, ref } from 'vue'
import { NButton, NDataTable, NSpin, NTag, useMessage, type DataTableColumns } from 'naive-ui'
import {
  getWalkForward,
  type WalkForwardReport,
  type WFMonthlyItem,
  type WFMonthlyRow,
  type WFSectionReport,
  type WFSegRow,
} from '@/api/admin'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import { useUi } from '@/composables/useUi'

const message = useMessage()
const { upColor, downColor } = useUi()

const report = ref<WalkForwardReport | null>(null)
const loading = ref(false)

async function load(refresh: boolean) {
  loading.value = true
  try {
    report.value = await getWalkForward(refresh)
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}
onMounted(() => void load(false))

function secTitle(sec: WFSectionReport): string {
  const holds = sec.holds.join('/')
  return sec.rec_type === 'short_term' ? `短线（持有 ${holds} 日）` : `长线（持有 ${holds} 日）`
}

const SEGMENT_LABEL: Record<string, string> = { val: '验证', test: '测试' }
const STATUS_LABEL: Record<string, string> = {
  traded: '成交',
  skip_limit_up: '一字板',
  skip_cash: '不足一手',
  skip_suspend: '停牌',
  pending: '未走完',
}

function pctColor(v: number): string | undefined {
  if (v > 0) return upColor.value
  if (v < 0) return downColor.value
  return undefined
}

function pctSpan(v: number, suffix = '%') {
  return h('span', { class: 'qv-tnum', style: `color:${pctColor(v) || 'inherit'}` }, `${v.toFixed(2)}${suffix}`)
}

function foldColumns(): DataTableColumns<WFSectionReport['folds'] extends (infer T)[] | null ? T : never> {
  return [
    { title: '折', key: 'fold', width: 50 },
    { title: '训练段', key: 'train', width: 190, render: (r) => h('span', { class: 'qv-tnum wf-range' }, `${r.train_range[0]} ~ ${r.train_range[1]}`) },
    { title: '验证段', key: 'val', width: 190, render: (r) => h('span', { class: 'qv-tnum wf-range' }, `${r.val_range[0]} ~ ${r.val_range[1]}`) },
    { title: '测试段', key: 'test', width: 190, render: (r) => h('span', { class: 'qv-tnum wf-range' }, `${r.test_range[0]} ~ ${r.test_range[1]}`) },
    { title: '信号日(验/测)', key: 'signals', width: 110, render: (r) => `${r.val_signals} / ${r.test_signals}` },
  ]
}

function rowColumns(): DataTableColumns<WFSegRow> {
  return [
    { title: '折', key: 'fold', width: 60, render: (r) => (r.fold === 0 ? h(NTag, { size: 'tiny', bordered: false }, { default: () => '合并' }) : String(r.fold)) },
    { title: '段', key: 'segment', width: 60, render: (r) => SEGMENT_LABEL[r.segment] || r.segment },
    { title: '策略', key: 'strategy_name', width: 100 },
    { title: '持有', key: 'hold', width: 60, render: (r) => `${r.hold}日` },
    { title: '信号', key: 'signals', width: 60 },
    { title: '成交/跳过', key: 'trades', width: 90, render: (r) => `${r.trades} / ${r.skipped}${r.pending ? ` (+${r.pending}未走完)` : ''}` },
    { title: 'Precision_net@K', key: 'pnet', width: 130, render: (r) => (r.trades ? pctSpan(r.precision_net_pct) : '—') },
    { title: '净收益中位', key: 'mnet', width: 100, render: (r) => (r.trades ? pctSpan(r.median_net_pct) : '—') },
    { title: '严重亏损率', key: 'severe', width: 100, render: (r) => (r.trades ? h('span', { class: 'qv-tnum', style: r.severe_loss_pct > 0 ? `color:${downColor.value}` : '' }, `${r.severe_loss_pct.toFixed(1)}%`) : '—') },
    { title: 'Precision_alpha@K', key: 'palpha', width: 140, render: (r) => (r.alpha_sample ? pctSpan(r.precision_alpha_pct) : '—') },
    { title: 'alpha中位', key: 'malpha', width: 90, render: (r) => (r.alpha_sample ? pctSpan(r.median_alpha_pct) : '—') },
  ]
}

function monthlyColumns(): DataTableColumns<WFMonthlyRow> {
  return [
    {
      type: 'expand',
      renderExpand: (row) =>
        h(
          'div',
          { class: 'wf-items' },
          (row.items || []).map((it: WFMonthlyItem) =>
            h('span', { key: it.symbol, class: 'wf-item' }, [
              h('span', null, `${it.name} ${it.symbol}`),
              h('span', { class: 'qv-tnum wf-item-score' }, ` 评分${it.score.toFixed(1)} `),
              it.status === 'traded' && it.net_pct != null
                ? pctSpan(it.net_pct)
                : h('span', { style: 'opacity:0.55' }, STATUS_LABEL[it.status] || it.status),
            ]),
          ),
        ),
    },
    { title: '月份', key: 'month', width: 80 },
    { title: '信号日', key: 'signal_date', width: 100, render: (r) => h('span', { class: 'qv-tnum' }, r.signal_date) },
    { title: '策略', key: 'strategy_name', width: 100 },
    { title: '持有', key: 'hold', width: 60, render: (r) => `${r.hold}日` },
    { title: '成交/跳过', key: 'trades', width: 90, render: (r) => `${r.trades} / ${r.skipped}${r.pending ? ` (+${r.pending})` : ''}` },
    { title: 'Precision_net', key: 'pnet', width: 110, render: (r) => (r.trades ? pctSpan(r.precision_net_pct) : '—') },
    { title: '净收益中位', key: 'mnet', width: 100, render: (r) => (r.trades ? pctSpan(r.median_net_pct) : '—') },
    { title: '严重亏损率', key: 'severe', width: 100, render: (r) => (r.trades ? `${r.severe_loss_pct.toFixed(1)}%` : '—') },
    { title: 'alpha中位', key: 'malpha', width: 90, render: (r) => (r.alpha_sample ? pctSpan(r.median_alpha_pct) : '—') },
  ]
}

function specLine(sec: WFSectionReport): string {
  const s = sec.spec
  return `训练 ${s.train} / 验证 ${s.val} / 测试 ${s.test} 交易日，步长 ${s.step}，Purge ${s.purge} + Embargo ${s.embargo}`
}
</script>

<template>
  <PageContainer
    title="Walk-Forward 基线"
    subtitle="S3-5 评估基线：手工评分（五维+策略加分）按历史 as-of 切片重放，训练/验证/测试滚动切分；纯测量不改写任何推荐行为"
  >
    <SectionCard title="评估概览">
      <template #extra>
        <div class="wf-toolbar">
          <span v-if="report" class="wf-meta">
            数据末日 {{ report.trade_date }} · Top{{ report.top_k }} 组合 · 宇宙 {{ report.universe }} 只 · 耗时
            {{ (report.elapsed_ms / 1000).toFixed(1) }}s
          </span>
          <n-button size="small" :loading="loading" @click="load(true)">重新计算</n-button>
        </div>
      </template>
      <n-spin :show="loading">
        <div v-if="report" class="wf-notes">
          <div v-for="(n, i) in report.notes" :key="i">{{ n }}</div>
          <div v-if="report.st_skipped || report.adjust_suspect">
            已剔除：ST {{ report.st_skipped }} 只、复权断层 {{ report.adjust_suspect }} 只。
          </div>
        </div>
        <div v-else-if="!loading" class="wf-empty">暂无数据：需全市场日线就绪后点「重新计算」（每信号日一次全市场重算，约数十秒）。</div>
      </n-spin>
    </SectionCard>

    <template v-if="report">
      <SectionCard v-for="sec in report.sections || []" :key="sec.rec_type" :title="secTitle(sec)">
        <div class="wf-spec">
          <n-tag v-if="sec.adapted" size="small" type="warning" :bordered="false">窗口已缩放</n-tag>
          <n-tag v-else size="small" type="success" :bordered="false">目标窗口</n-tag>
          <span class="qv-tnum">{{ specLine(sec) }}</span>
        </div>
        <div class="wf-spec-note">{{ sec.spec_note }}</div>

        <template v-if="sec.folds?.length">
          <div class="wf-sub">切分（{{ sec.folds.length }} 折，右对齐保证最新数据被测试）</div>
          <n-data-table :columns="foldColumns()" :data="sec.folds" :row-key="(r: any) => r.fold" size="small" :scroll-x="740" />
          <div class="wf-sub">评估指标（Precision_net@K 与 Precision_alpha@K 分开；净收益中位数与严重亏损率 net&lt;-5% 并列）</div>
          <n-data-table
            :columns="rowColumns()"
            :data="sec.rows || []"
            :row-key="(r: WFSegRow) => `${r.fold}-${r.segment}-${r.strategy}-${r.hold}`"
            size="small"
            :scroll-x="1000"
          />
        </template>

        <template v-if="sec.monthly?.length">
          <div class="wf-sub">评分 Top{{ report.top_k }} 组合月度走查（每月首个交易日建仓，点行首展开组合成员）</div>
          <n-data-table
            :columns="monthlyColumns()"
            :data="sec.monthly"
            :row-key="(r: WFMonthlyRow) => `${r.month}-${r.strategy}`"
            size="small"
            :scroll-x="920"
            :max-height="420"
          />
        </template>
      </SectionCard>
    </template>
  </PageContainer>
</template>

<style scoped>
.wf-toolbar {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}
.wf-meta {
  font-size: 12px;
  opacity: 0.6;
}
.wf-empty {
  padding: 24px 0;
  opacity: 0.6;
  font-size: 13px;
}
.wf-notes {
  font-size: 12px;
  opacity: 0.6;
  line-height: 1.8;
}
.wf-spec {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  font-size: 13px;
}
.wf-spec-note {
  margin: 6px 0 4px;
  font-size: 12px;
  opacity: 0.6;
  line-height: 1.7;
}
.wf-sub {
  margin: 14px 0 8px;
  font-size: 13px;
  font-weight: 600;
}
.wf-range {
  font-size: 12px;
}
.wf-items {
  display: flex;
  flex-wrap: wrap;
  gap: 6px 14px;
  padding: 4px 0;
}
.wf-item {
  font-size: 12px;
  white-space: nowrap;
}
.wf-item-score {
  opacity: 0.6;
}
</style>
