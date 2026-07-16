<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import { NButton, NDataTable, NSpin, NTag, useMessage, type DataTableColumns } from 'naive-ui'
import { getFactorIC, type FactorICReport, type FactorICStat } from '@/api/admin'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import { useUi } from '@/composables/useUi'

const message = useMessage()
const { upColor, downColor } = useUi()

const report = ref<FactorICReport | null>(null)
const loading = ref(false)

async function load(refresh: boolean) {
  loading.value = true
  try {
    report.value = await getFactorIC(refresh)
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}
onMounted(() => void load(false))

function icColor(v: number): string | undefined {
  if (v > 0.02) return upColor.value
  if (v < -0.02) return downColor.value
  return undefined
}

function icCell(row: FactorICStat, hz: string) {
  const a = row.horizons[hz]
  if (!a) return h('span', { style: 'opacity:0.4' }, '—')
  return h('span', { class: 'qv-tnum', style: `color:${icColor(a.mean_ic) || 'inherit'}` }, [
    `${a.mean_ic.toFixed(4)} / ${a.icir.toFixed(2)} / ${a.win_rate_pct.toFixed(0)}%`,
  ])
}

const columns = computed<DataTableColumns<FactorICStat>>(() => [
  {
    title: '因子',
    key: 'name',
    width: 170,
    render: (row) =>
      h('span', null, [
        h('span', null, row.name + ' '),
        h(NTag, { size: 'tiny', bordered: false, round: true }, { default: () => row.group }),
      ]),
  },
  { title: 'key', key: 'key', width: 130, render: (row) => h('code', { style: 'font-size:12px;opacity:0.7' }, row.key) },
  { title: '5日 IC/ICIR/胜率', key: 'h5', width: 170, render: (row) => icCell(row, '5') },
  { title: '10日 IC/ICIR/胜率', key: 'h10', width: 170, render: (row) => icCell(row, '10') },
  { title: '20日 IC/ICIR/胜率', key: 'h20', width: 170, render: (row) => icCell(row, '20') },
])
</script>

<template>
  <PageContainer title="因子 IC 排行" subtitle="S3-4 RankIC 验证：A 类因子按历史日线 as-of 重建 × 未来 5/10/20 日收益的 Spearman 秩相关（只读报表，不做删改判定）">
    <SectionCard title="RankIC 排行（按 |10日 IC 均值| 降序）">
      <template #extra>
        <div class="ic-toolbar">
          <span v-if="report" class="ic-meta">
            数据末日 {{ report.trade_date }} · {{ report.dates.length }} 个横截面 · 宇宙 {{ report.universe }} 只 · 耗时
            {{ report.elapsed_ms }}ms
          </span>
          <n-button size="small" :loading="loading" @click="load(true)">重新计算</n-button>
        </div>
      </template>
      <n-spin :show="loading">
        <n-data-table
          v-if="report?.stats?.length"
          :columns="columns"
          :data="report.stats"
          :row-key="(r: FactorICStat) => r.key"
          size="small"
          :scroll-x="820"
        />
        <div v-else-if="!loading" class="ic-empty">暂无数据：需全市场日线就绪后点「重新计算」。</div>
        <div v-if="report" class="ic-notes">
          <div v-for="(n, i) in report.notes" :key="i">{{ n }}</div>
          <div v-if="report.st_skipped || report.adjust_suspect">
            已剔除：ST {{ report.st_skipped }} 只、复权断层 {{ report.adjust_suspect }} 只；单横截面有效样本 &lt;{{ report.min_cross }} 不计当日 IC。
          </div>
        </div>
      </n-spin>
    </SectionCard>
  </PageContainer>
</template>

<style scoped>
.ic-toolbar {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}
.ic-meta {
  font-size: 12px;
  opacity: 0.6;
}
.ic-empty {
  padding: 24px 0;
  opacity: 0.6;
  font-size: 13px;
}
.ic-notes {
  margin-top: 12px;
  font-size: 12px;
  opacity: 0.6;
  line-height: 1.8;
}
</style>
