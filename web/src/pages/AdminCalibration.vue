<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import { NButton, NDataTable, NSpin, NTag, useMessage, type DataTableColumns } from 'naive-ui'
import {
  getLLMCalibration,
  type LLMCalibrationReport,
  type RecCalibReport,
  type CalibBucket,
  type CalibTierCell,
  type AnalysisCalibTier,
} from '@/api/admin'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import { useUi } from '@/composables/useUi'

const message = useMessage()
const { upColor, downColor } = useUi()

const report = ref<LLMCalibrationReport | null>(null)
const loading = ref(false)

async function load(refresh: boolean) {
  loading.value = true
  try {
    report.value = await getLLMCalibration(refresh)
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}
onMounted(() => void load(false))

const typeLabel: Record<string, string> = { short_term: '短线', long_term: '长线' }

function pct(v?: number): string {
  return v === undefined || v === null ? '—' : `${v.toFixed(2)}%`
}
function num4(v?: number): string {
  return v === undefined || v === null ? '未评估' : v.toFixed(4)
}
function netColor(v: number): string | undefined {
  if (v > 0) return upColor.value
  if (v < 0) return downColor.value
  return undefined
}

const tierColumns = computed<DataTableColumns<CalibTierCell>>(() => [
  {
    title: '档位',
    key: 'tier',
    width: 90,
    render: (r) =>
      h(
        NTag,
        { size: 'small', bordered: false, type: r.tier === 'high' ? 'success' : r.tier === 'low' ? 'warning' : 'default' },
        { default: () => r.tier },
      ),
  },
  {
    title: '样本',
    key: 'sample',
    width: 76,
    render: (r) => h('span', { class: 'qv-tnum', style: r.sample < 5 ? 'opacity:0.5' : '' }, r.sample < 5 ? `${r.sample}（不足）` : String(r.sample)),
  },
  { title: '命中率', key: 'hit', width: 84, render: (r) => h('span', { class: 'qv-tnum' }, pct(r.hit_rate_pct)) },
  { title: '中位净收益', key: 'med', width: 100, render: (r) => h('span', { class: 'qv-tnum', style: `color:${netColor(r.median_net_pct) || 'inherit'}` }, pct(r.median_net_pct)) },
  { title: '均值净收益', key: 'net', width: 100, render: (r) => h('span', { class: 'qv-tnum', style: `color:${netColor(r.avg_net_pct) || 'inherit'}` }, pct(r.avg_net_pct)) },
  { title: '均值毛收益', key: 'gross', width: 100, render: (r) => h('span', { class: 'qv-tnum' }, pct(r.avg_gross_pct)) },
  { title: '均值α', key: 'alpha', width: 96, render: (r) => h('span', { class: 'qv-tnum' }, r.alpha_sample > 0 ? pct(r.avg_alpha_pct) : '—') },
  { title: '严重亏损率', key: 'severe', width: 96, render: (r) => h('span', { class: 'qv-tnum' }, pct(r.severe_loss_pct)) },
])

const bucketColumns = computed<DataTableColumns<CalibBucket>>(() => [
  { title: '口头置信度桶', key: 'label', width: 110 },
  { title: '样本', key: 'sample', width: 70, render: (r) => h('span', { class: 'qv-tnum', style: r.sample < 5 ? 'opacity:0.5' : '' }, String(r.sample)) },
  { title: '平均置信度', key: 'conf', width: 96, render: (r) => h('span', { class: 'qv-tnum' }, r.avg_conf.toFixed(1)) },
  { title: '实际命中率', key: 'hit', width: 96, render: (r) => h('span', { class: 'qv-tnum' }, pct(r.hit_rate_pct)) },
  {
    title: '偏差（命中−置信）',
    key: 'gap',
    width: 130,
    render: (r) =>
      h('span', { class: 'qv-tnum', style: `color:${r.gap_pct < 0 ? downColor.value : upColor.value}` }, `${r.gap_pct > 0 ? '+' : ''}${r.gap_pct.toFixed(1)}pt`),
  },
  { title: '平均净收益', key: 'net', width: 96, render: (r) => h('span', { class: 'qv-tnum', style: `color:${netColor(r.avg_net_pct) || 'inherit'}` }, pct(r.avg_net_pct)) },
])

const anaTierColumns = computed<DataTableColumns<AnalysisCalibTier>>(() => [
  { title: '档位', key: 'tier', width: 90, render: (r) => h(NTag, { size: 'small', bordered: false, type: r.tier === 'high' ? 'success' : r.tier === 'low' ? 'warning' : 'default' }, { default: () => r.tier }) },
  { title: '样本', key: 'sample', width: 76, render: (r) => h('span', { class: 'qv-tnum', style: r.sample < 5 ? 'opacity:0.5' : '' }, String(r.sample)) },
  { title: '方向命中率', key: 'hit', width: 96, render: (r) => h('span', { class: 'qv-tnum' }, pct(r.hit_rate_pct)) },
  { title: '+20 日平均收益', key: 'ret', width: 120, render: (r) => h('span', { class: 'qv-tnum', style: `color:${netColor(r.avg_ret20_pct) || 'inherit'}` }, pct(r.avg_ret20_pct)) },
])

function prLine(rep: RecCalibReport): string {
  const pr = rep.action_pr
  const parts: string[] = []
  parts.push(`precision(净收益)=${pct(pr.precision_net_pct)}`)
  parts.push(`recall=${pct(pr.recall_net_pct)}`)
  parts.push(`watch 对照命中=${pct(pr.watch_hit_pct)}`)
  if (pr.alpha_sample > 0) parts.push(`precision(α)=${pct(pr.precision_alpha_pct)}`)
  if (pr.alpha_sample > 0) parts.push(`recall(α)=${pct(pr.recall_alpha_pct)}`)
  return parts.join(' · ')
}
</script>

<template>
  <PageContainer
    title="LLM 校准报表"
    subtitle="P1-7 校准与后验标签：程序合成置信度分档命中率 + 模型口头置信度 Brier/ECE/可靠性曲线（纯测量零门控；口头置信度不当真实概率，本表是其校准性证据）"
  >
    <div class="calib-wrap">
      <SectionCard title="推荐置信度 × 后验标签">
        <template #extra>
          <div class="calib-toolbar">
            <span v-if="report" class="calib-meta">
              标签口径 {{ report.label_version }} · {{ report.generated_at }} · 耗时 {{ report.elapsed_ms }}ms
            </span>
            <n-button size="small" :loading="loading" @click="load(true)">重新计算</n-button>
          </div>
        </template>
        <n-spin :show="loading">
          <div v-if="report">
            <div v-for="rec in report.recommendation" :key="rec.type" class="calib-block">
              <div class="calib-head">
                <span class="calib-title">{{ typeLabel[rec.type] || rec.type }} · 持有 {{ rec.horizon_days }} 交易日</span>
                <n-tag size="small" :type="rec.evaluated ? 'success' : 'warning'" :bordered="false">
                  {{ rec.evaluated ? `已评估（buy 校准样本达标）` : `未评估（buy 校准样本不足 100）` }}
                </n-tag>
                <span class="calib-kv">Brier={{ num4(rec.brier) }} · ECE={{ num4(rec.ece) }}</span>
              </div>
              <div class="calib-cov">
                标签覆盖：共 {{ rec.coverage.total }} 条 · 成熟 {{ rec.coverage.matured }}（{{ rec.coverage.matured_ratio_pct.toFixed(1) }}%）·
                待成熟 {{ rec.coverage.pending }} · 无法成交 {{ rec.coverage.skipped }} · 无数据 {{ rec.coverage.no_data }} ·
                强平剔除 {{ rec.coverage.forced }} · 量化降级剔除 {{ rec.coverage.degraded_excl }} · 孤儿剔除 {{ rec.coverage.orphan_excl }}
              </div>
              <div class="calib-cov">动作判定（buy {{ rec.buy_sample }} / 全部 {{ rec.action_pr.sample }}）：{{ prLine(rec) }}</div>
              <template v-if="rec.sys_tiers?.length">
                <div class="calib-sub">程序合成置信度分档（仅 buy；成本前/后收益分开）</div>
                <n-data-table :columns="tierColumns" :data="rec.sys_tiers" :row-key="(r: CalibTierCell) => r.tier" size="small" :scroll-x="760" />
                <div v-if="rec.tier_monotone" class="calib-note">{{ rec.tier_monotone }}</div>
              </template>
              <template v-if="rec.reliability?.length">
                <div class="calib-sub">口头置信度可靠性曲线（仅 buy；负偏差=过度自信）</div>
                <n-data-table :columns="bucketColumns" :data="rec.reliability" :row-key="(r: CalibBucket) => r.label" size="small" :scroll-x="620" />
              </template>
              <div class="calib-notes">
                <div v-for="(n, i) in rec.notes" :key="i">{{ n }}</div>
              </div>
            </div>
          </div>
          <div v-else-if="!loading" class="calib-empty">暂无数据：点「重新计算」生成（需已积累成熟标签）。</div>
        </n-spin>
      </SectionCard>

      <SectionCard title="分析评级方向 × 事后走势">
        <n-spin :show="loading">
          <div v-if="report?.analysis">
            <div class="calib-head">
              <n-tag size="small" :type="report.analysis.evaluated ? 'success' : 'warning'" :bordered="false">
                {{ report.analysis.evaluated ? `已评估（可判定 ${report.analysis.judged}）` : `未评估（可判定 ${report.analysis.judged} 不足 100）` }}
              </n-tag>
              <span class="calib-kv">Brier={{ num4(report.analysis.brier) }} · ECE={{ num4(report.analysis.ece) }}</span>
            </div>
            <div class="calib-cov">
              回看 {{ report.analysis.scanned }} 条个股标准分析 · 可判定 {{ report.analysis.judged }} ·
              中性不判 {{ report.analysis.neutral_skipped }} · 未满 20 交易日 {{ report.analysis.immature_skipped }} ·
              无本地日线 {{ report.analysis.no_data_skipped }} · 旧记录无程序置信度 {{ report.analysis.no_sys_conf }} · 同标的同日去重 {{ report.analysis.dup_skipped }}
            </div>
            <template v-if="report.analysis.sys_tiers?.length">
              <div class="calib-sub">程序合成置信度分档（方向命中口径）</div>
              <n-data-table :columns="anaTierColumns" :data="report.analysis.sys_tiers" :row-key="(r: AnalysisCalibTier) => r.tier" size="small" :scroll-x="420" />
            </template>
            <template v-if="report.analysis.reliability?.length">
              <div class="calib-sub">口头置信度可靠性曲线（方向命中口径）</div>
              <n-data-table :columns="bucketColumns" :data="report.analysis.reliability" :row-key="(r: CalibBucket) => r.label" size="small" :scroll-x="620" />
            </template>
            <div class="calib-notes">
              <div v-for="(n, i) in report.analysis.notes" :key="i">{{ n }}</div>
              <div v-for="(n, i) in report.notes" :key="'g' + i">{{ n }}</div>
            </div>
          </div>
        </n-spin>
      </SectionCard>
    </div>
  </PageContainer>
</template>

<style scoped>
.calib-wrap {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.calib-toolbar {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}
.calib-meta {
  font-size: 12px;
  opacity: 0.6;
}
.calib-block + .calib-block {
  margin-top: 24px;
  padding-top: 16px;
  border-top: 1px dashed var(--qv-border, rgba(128, 128, 128, 0.2));
}
.calib-head {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  margin-bottom: 6px;
}
.calib-title {
  font-weight: 600;
}
.calib-kv {
  font-size: 12px;
  opacity: 0.7;
}
.calib-cov {
  font-size: 12px;
  opacity: 0.65;
  margin: 2px 0;
}
.calib-sub {
  font-size: 13px;
  font-weight: 600;
  margin: 12px 0 6px;
}
.calib-note {
  font-size: 12px;
  opacity: 0.65;
  margin-top: 6px;
}
.calib-notes {
  margin-top: 10px;
  font-size: 12px;
  opacity: 0.55;
  line-height: 1.8;
}
.calib-empty {
  padding: 24px 0;
  opacity: 0.6;
  font-size: 13px;
}
</style>
