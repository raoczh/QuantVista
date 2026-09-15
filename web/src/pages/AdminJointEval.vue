<script setup lang="ts">
import { computed, h, onBeforeUnmount, onMounted, ref } from 'vue'
import { NAlert, NButton, NDataTable, NPopconfirm, NSpin, NTag, useMessage, type DataTableColumns } from 'naive-ui'
import {
  getJointEval,
  type JointEvalReport,
  type JointEvalSection,
  type JointEvalSegment,
  type CalibSliceRow,
} from '@/api/admin'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import { useUi } from '@/composables/useUi'
import { getSessionEpoch } from '@/api/token'

const message = useMessage()
const { upColor, downColor } = useUi()

const report = ref<JointEvalReport | null>(null)
const loading = ref(false)
const loadError = ref('')
const pageSession = getSessionEpoch()
let disposed = false
const pageActive = () => !disposed && getSessionEpoch() === pageSession
onBeforeUnmount(() => { disposed = true })

async function load(opts: { refresh?: boolean; includeLocked?: boolean } = {}) {
  if (!pageActive() || loading.value) return
  loading.value = true
  loadError.value = ''
  try {
    const next = await getJointEval(opts)
    if (!pageActive()) return
    report.value = next
    if (opts.includeLocked) {
      if (next.sections.some((section) => section.locked)) {
        message.warning('已读取锁定测试段并登记审计（调参迭代请只看开发段）')
      } else {
        message.info('当前没有可读取的锁定测试段')
      }
    }
  } catch (e) {
    if (pageActive()) loadError.value = e instanceof Error ? e.message : '联合评估读取失败'
  } finally {
    loading.value = false
  }
}
onMounted(() => void load())

const typeLabel: Record<string, string> = { short_term: '短线', long_term: '长线' }

function pct(v?: number): string {
  return v === undefined || v === null ? '—' : `${v.toFixed(2)}%`
}
function netColor(v: number): string | undefined {
  if (v > 0) return upColor.value
  if (v < 0) return downColor.value
  return undefined
}

// 段指标行化：dev / locked 两行同一列结构（locked 未请求时不出行）。
interface SegRow extends JointEvalSegment {
  seg_label: string
}
function segRows(sec: JointEvalSection): SegRow[] {
  const rows: SegRow[] = []
  if (sec.dev) rows.push({ ...sec.dev, seg_label: '开发段（dev）' })
  if (sec.locked) rows.push({ ...sec.locked, seg_label: '锁定测试段' })
  return rows
}

const segColumns = computed<DataTableColumns<SegRow>>(() => [
  { title: '段', key: 'seg_label', width: 120 },
  { title: '信号日', key: 'signal_days', width: 76, render: (r) => h('span', { class: 'qv-tnum' }, `${r.signal_days} 天`) },
  { title: '范围', key: 'range', width: 175, render: (r) => h('span', { class: 'qv-tnum', style: 'font-size:12px' }, `${r.date_start} ~ ${r.date_end}`) },
  { title: 'buy 样本', key: 'buy_sample', width: 84, render: (r) => h('span', { class: 'qv-tnum' }, String(r.buy_sample)) },
  { title: '胜率', key: 'win', width: 80, render: (r) => h('span', { class: 'qv-tnum' }, pct(r.win_rate_pct)) },
  { title: '均值净收益', key: 'net', width: 96, render: (r) => h('span', { class: 'qv-tnum', style: `color:${netColor(r.avg_net_pct) || 'inherit'}` }, pct(r.avg_net_pct)) },
  { title: '中位', key: 'med', width: 84, render: (r) => h('span', { class: 'qv-tnum', style: `color:${netColor(r.median_net_pct) || 'inherit'}` }, pct(r.median_net_pct)) },
  { title: 'P10', key: 'p10', width: 84, render: (r) => h('span', { class: 'qv-tnum', style: `color:${netColor(r.p10_net_pct) || 'inherit'}` }, pct(r.p10_net_pct)) },
  { title: '严重亏损率', key: 'severe', width: 96, render: (r) => h('span', { class: 'qv-tnum' }, pct(r.severe_loss_pct)) },
  { title: '均值α', key: 'alpha', width: 84, render: (r) => h('span', { class: 'qv-tnum' }, r.alpha_sample > 0 ? pct(r.avg_alpha_pct) : '—') },
  { title: '成本拖累', key: 'cost', width: 88, render: (r) => h('span', { class: 'qv-tnum' }, pct(r.cost_drag_pct)) },
  { title: '串联净值', key: 'nav', width: 88, render: (r) => h('span', { class: 'qv-tnum', style: `color:${netColor(r.nav_return_pct) || 'inherit'}` }, pct(r.nav_return_pct)) },
  { title: '最大回撤', key: 'dd', width: 88, render: (r) => h('span', { class: 'qv-tnum', style: r.max_drawdown_pct > 0 ? `color:${downColor.value}` : '' }, pct(r.max_drawdown_pct)) },
  { title: '均值 MAE', key: 'mae', width: 88, render: (r) => h('span', { class: 'qv-tnum' }, pct(r.avg_mae_pct)) },
  { title: '最差 MAE', key: 'wmae', width: 88, render: (r) => h('span', { class: 'qv-tnum' }, pct(r.worst_mae_pct)) },
  { title: 'Brier', key: 'brier', width: 84, render: (r) => h('span', { class: 'qv-tnum' }, r.brier === undefined || r.brier === null ? '未评估' : r.brier.toFixed(4)) },
  { title: 'ECE', key: 'ece', width: 84, render: (r) => h('span', { class: 'qv-tnum' }, r.ece === undefined || r.ece === null ? '未评估' : r.ece.toFixed(4)) },
])

const sliceColumns = computed<DataTableColumns<CalibSliceRow>>(() => [
  { title: '取值', key: 'key', width: 200, ellipsis: { tooltip: true } },
  { title: '样本', key: 'sample', width: 70, render: (r) => h('span', { class: 'qv-tnum', style: r.sample < 5 ? 'opacity:0.5' : '' }, String(r.sample)) },
  { title: '命中率', key: 'hit', width: 84, render: (r) => h('span', { class: 'qv-tnum' }, pct(r.hit_rate_pct)) },
  { title: '均值净收益', key: 'net', width: 100, render: (r) => h('span', { class: 'qv-tnum', style: `color:${netColor(r.avg_net_pct) || 'inherit'}` }, pct(r.avg_net_pct)) },
  { title: '均值α', key: 'alpha', width: 90, render: (r) => h('span', { class: 'qv-tnum' }, r.alpha_sample > 0 ? pct(r.avg_alpha_pct) : '—') },
  { title: 'Brier', key: 'brier', width: 90, render: (r) => h('span', { class: 'qv-tnum' }, r.brier === undefined || r.brier === null ? '未评估' : r.brier.toFixed(4)) },
])
</script>

<template>
  <PageContainer
    title="联合评估"
    subtitle="联合比较收益、回撤、换手和成本；开发区间与锁定测试区间分开使用。"
  >
    <div class="je-wrap">
      <SectionCard title="联合评估">
        <template #extra>
          <div class="je-toolbar">
            <span v-if="report" class="je-meta">
              标签口径 {{ report.label_version }} · {{ report.generated_at }} · 耗时 {{ report.elapsed_ms }}ms
            </span>
            <n-tag v-if="report?.locked_audit?.count" size="small" type="warning" :bordered="false">
              锁定段已读 {{ report.locked_audit.count }} 次（最近 {{ report.locked_audit.last_at }}）
            </n-tag>
            <n-button size="small" :loading="loading" :disabled="loading" @click="load({ refresh: true })">重新计算</n-button>
            <n-popconfirm :disabled="loading" :positive-button-props="{ loading, disabled: loading }" @positive-click="load({ includeLocked: true })">
              <template #trigger>
                <n-button size="small" type="warning" :loading="loading" :disabled="loading">读取锁定段</n-button>
              </template>
              锁定测试段留给发布前验收，每次读取都会登记审计（当前已读
              {{ report?.locked_audit?.count || 0 }} 次）。调参迭代只看开发段——确定读取？
            </n-popconfirm>
          </div>
        </template>
        <n-spin :show="loading">
          <n-alert v-if="loadError" type="error" :show-icon="false">{{ loadError }}</n-alert>
          <div v-if="report">
            <div v-for="sec in report.sections" :key="sec.type" class="je-block">
              <div class="je-head">
                <span class="je-title">{{ typeLabel[sec.type] || sec.type }} · 持有 {{ sec.horizon_days }} 交易日</span>
                <!-- 这段是一整句带两个百分数的说明（约 420px），不是标签语义：
                     n-tag 写死 white-space:nowrap + 固定 height，375px 下必然溢出并让
                     整个卡片内容区出现横滚。用普通文本节点承载。 -->
                <span class="je-turnover">
                  换手{{ report.include_locked ? '（全量日期）' : '（开发段日期界内）' }}：相邻批次新进 {{ sec.turnover.pairs > 0 ? pct(sec.turnover.avg_new_pct) : '—' }} · 重合
                  {{ sec.turnover.pairs > 0 ? pct(sec.turnover.avg_overlap_pct) : '—' }}（{{ sec.turnover.pairs }} 对）
                </span>
              </div>
              <div class="je-cov">
                标签覆盖：共 {{ sec.coverage.total }} 条 · 成熟 {{ sec.coverage.matured }}（{{ sec.coverage.matured_ratio_pct.toFixed(1) }}%）·
                待成熟 {{ sec.coverage.pending }} · 无法成交 {{ sec.coverage.skipped }} · 无数据 {{ sec.coverage.no_data }} ·
                强平剔除 {{ sec.coverage.forced }} · 降级剔除 {{ sec.coverage.degraded_excl }} · 孤儿剔除 {{ sec.coverage.orphan_excl }}
              </div>
              <template v-if="segRows(sec).length">
                <n-data-table :columns="segColumns" :data="segRows(sec)" :row-key="(r: SegRow) => r.seg_label" size="small" :scroll-x="1620" />
              </template>
              <div v-else class="je-empty">暂无成熟样本。</div>
              <template v-for="seg in [sec.dev, sec.locked]" :key="sec.type + (seg?.segment || '')">
                <div v-if="seg?.raw_calib" class="je-cov">
                  {{ seg.segment === 'locked' ? '锁定段' : '开发段' }}原始口径（复核改写前）：快照样本 {{ seg.raw_calib.sample }} ·
                  无快照 {{ seg.raw_calib.missing }} · 被复核改写 {{ seg.raw_calib.diverged }} ·
                  Brier={{ seg.raw_calib.brier === undefined || seg.raw_calib.brier === null ? '未评估' : seg.raw_calib.brier.toFixed(4) }} ·
                  ECE={{ seg.raw_calib.ece === undefined || seg.raw_calib.ece === null ? '未评估' : seg.raw_calib.ece.toFixed(4) }}（表内 Brier/ECE 为终值口径）
                </div>
              </template>
              <div v-if="sec.locked_preview && !sec.locked" class="je-locked">
                锁定测试段：{{ sec.locked_preview.date_start }} ~ {{ sec.locked_preview.date_end }}（{{ sec.locked_preview.signal_days }} 个信号日 ·
                {{ sec.locked_preview.sample }} 样本）——指标未读取（点「读取锁定段」显式请求，读取将登记审计）
              </div>
              <template v-for="grp in sec.slices || []" :key="grp.dim">
                <div class="je-sub">对照：{{ grp.label }}（只吃开发段 buy 样本——锁定段隔离）</div>
                <n-data-table :columns="sliceColumns" :data="grp.rows" :row-key="(r: CalibSliceRow) => r.key" size="small" :scroll-x="640" />
              </template>
              <div class="je-notes">
                <div v-for="(n, i) in sec.notes" :key="i">{{ n }}</div>
              </div>
            </div>
            <div class="je-notes">
              <div v-for="(n, i) in report.notes" :key="'g' + i">{{ n }}</div>
            </div>
          </div>
          <div v-else-if="!loading && !loadError" class="je-empty">暂无数据：点「重新计算」生成（需已积累成熟标签）。</div>
        </n-spin>
      </SectionCard>
    </div>
  </PageContainer>
</template>

<style scoped>
.je-wrap {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.je-toolbar {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}
.je-meta {
  font-size: 12px;
  opacity: 0.6;
}
.je-block + .je-block {
  margin-top: 24px;
  padding-top: 16px;
  border-top: 1px dashed var(--qv-border, rgba(128, 128, 128, 0.2));
}
.je-head {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  margin-bottom: 6px;
}
.je-title {
  font-weight: 600;
  min-width: 0;
  overflow-wrap: anywhere;
}
.je-turnover {
  flex: 1 1 260px;
  min-width: 0;
  font-size: 12px;
  line-height: 1.55;
  opacity: 0.7;
  overflow-wrap: anywhere;
}
.je-cov {
  font-size: 12px;
  opacity: 0.65;
  /* 上边距不能收到 2px：本类既用在 .je-head 之后（那里已有 mb:6），
   * 也用在 n-data-table 之后（原始口径说明），后者只有 2px 会贴住表格底边。 */
  margin: 8px 0;
  overflow-wrap: anywhere;
}
.je-sub {
  font-size: 13px;
  font-weight: 600;
  margin: 12px 0 6px;
}
.je-locked {
  font-size: 12px;
  opacity: 0.75;
  margin-top: 8px;
  padding: 8px 10px;
  border: 1px dashed var(--qv-border, rgba(128, 128, 128, 0.3));
  border-radius: 8px;
  overflow-wrap: anywhere;
}
.je-notes {
  margin-top: 10px;
  font-size: 12px;
  opacity: 0.55;
  line-height: 1.8;
  overflow-wrap: anywhere;
}
.je-empty {
  padding: 24px 0;
  opacity: 0.6;
  font-size: 13px;
}
/* 卡头 extra 里的 meta 与「锁定段已读…（时间戳）」标签在窄屏会把按钮挤出视口 */
@media (max-width: 768px) {
  .je-meta {
    width: 100%;
  }
  .je-toolbar > :deep(.n-tag) {
    max-width: 100%;
    height: auto;
    white-space: normal;
    line-height: 1.5;
    padding-block: 3px;
  }
}
</style>
