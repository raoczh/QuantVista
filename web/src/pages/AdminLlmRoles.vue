<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { NAlert, NButton, NCollapse, NCollapseItem, NSpin, NTag } from 'naive-ui'
import { getLLMRoles, type LLMRoleAsset } from '@/api/admin'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import { getSessionEpoch } from '@/api/token'

const roles = ref<LLMRoleAsset[]>([])
const disciplines = ref<string[]>([])
const loading = ref(false)
const loadError = ref('')
const pageSession = getSessionEpoch()
let disposed = false
const pageActive = () => !disposed && getSessionEpoch() === pageSession
onBeforeUnmount(() => { disposed = true })

async function load() {
  if (!pageActive() || loading.value) return
  loading.value = true
  loadError.value = ''
  try {
    const res = await getLLMRoles()
    if (!pageActive()) return
    roles.value = res.roles
    disciplines.value = res.disciplines
  } catch (e) {
    if (pageActive()) loadError.value = e instanceof Error ? e.message : '角色表读取失败'
  } finally {
    loading.value = false
  }
}
onMounted(() => void load())
</script>

<template>
  <PageContainer
    title="LLM 角色资产 registry"
    subtitle="P1-8 系统内建角色的机读登记（只读声明表，与代码测试锁定一致）：版本锚 / schema / 触发条件 / 输入白名单 / 必答要求 / 禁止动作 / 预算 / 反例坐标"
  >
    <div class="roles-wrap">
      <n-alert v-if="loadError" type="error" :bordered="false">{{ loadError }} <n-button size="small" text @click="load">重试</n-button></n-alert>
      <SectionCard title="全局纪律（跨角色恒真）">
        <n-spin :show="loading">
          <ul class="roles-disc">
            <li v-for="(d, i) in disciplines" :key="i">{{ d }}</li>
          </ul>
        </n-spin>
      </SectionCard>

      <SectionCard :title="`角色卡（${roles.length} 个，与预算表键集一致）`">
        <n-spin :show="loading">
          <n-collapse v-if="roles.length" display-directive="show">
            <n-collapse-item v-for="r in roles" :key="r.role_id" :name="r.role_id">
              <template #header>
                <span class="role-head">
                  <code class="role-id">{{ r.role_id }}</code>
                  <span class="role-name">{{ r.name }}</span>
                  <n-tag size="tiny" :bordered="false" round>{{ r.version }}</n-tag>
                  <n-tag size="tiny" :bordered="false" round type="info">{{ r.schema_version }}</n-tag>
                  <span class="role-budget">预算 {{ r.max_tokens }} tok · repair ≤{{ r.repair_attempts }}</span>
                </span>
              </template>
              <div class="role-body">
                <div class="role-row"><span class="role-k">目标</span><span>{{ r.purpose }}</span></div>
                <div class="role-row"><span class="role-k">适用</span><span>市场 {{ r.market }} · {{ r.horizons }}</span></div>
                <div class="role-row"><span class="role-k">触发/路由</span><span>{{ r.trigger }}</span></div>
                <div class="role-row">
                  <span class="role-k">输入白名单</span>
                  <span><n-tag v-for="w in r.input_whitelist" :key="w" size="tiny" :bordered="false" class="role-tag">{{ w }}</n-tag></span>
                </div>
                <div class="role-row">
                  <span class="role-k">必答要求</span>
                  <ul class="role-list"><li v-for="m in r.must_answer" :key="m">{{ m }}</li></ul>
                </div>
                <div class="role-row">
                  <span class="role-k">禁止动作</span>
                  <ul class="role-list role-forbid"><li v-for="f in r.forbidden_actions" :key="f">{{ f }}</li></ul>
                </div>
                <div class="role-row"><span class="role-k">失败降级</span><span>{{ r.fallback }}</span></div>
                <div class="role-row">
                  <span class="role-k">known-answer</span>
                  <span><code v-for="c in r.known_answers" :key="c" class="role-ce">{{ c }}</code></span>
                </div>
                <div class="role-row">
                  <span class="role-k">edge-case</span>
                  <span><code v-for="c in r.edge_cases" :key="c" class="role-ce">{{ c }}</code></span>
                </div>
              </div>
            </n-collapse-item>
          </n-collapse>
          <div v-else-if="!loading && !loadError" class="roles-empty">暂无角色数据。</div>
        </n-spin>
      </SectionCard>
    </div>
  </PageContainer>
</template>

<style scoped>
.roles-wrap {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.roles-disc {
  margin: 0;
  padding-left: 18px;
  font-size: 13px;
  line-height: 1.9;
}
.role-head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  /* naive collapse 的 __header-main 只有 flex:1、没有 min-width:0 */
  min-width: 0;
}
.role-id {
  font-size: 12px;
  opacity: 0.75;
  overflow-wrap: anywhere;
}
.role-name {
  font-weight: 600;
  min-width: 0;
  overflow-wrap: anywhere;
}
.role-budget {
  font-size: 12px;
  opacity: 0.55;
}
.role-body {
  display: flex;
  flex-direction: column;
  gap: 8px;
  font-size: 13px;
}
.role-row {
  display: flex;
  gap: 10px;
  align-items: flex-start;
  min-width: 0;
}
.role-k {
  flex: 0 0 84px;
  font-size: 12px;
  opacity: 0.55;
  padding-top: 1px;
}
/* 值侧：内含 n-tag（naive 写死 nowrap），必须显式放开收缩，否则窄屏溢出 */
.role-row > :last-child {
  min-width: 0;
  overflow-wrap: anywhere;
}
.role-tag {
  margin: 0 6px 4px 0;
}
.role-list {
  margin: 0;
  padding-left: 16px;
  line-height: 1.7;
  overflow-wrap: anywhere;
}
.role-forbid li {
  opacity: 0.85;
}
.role-ce {
  font-size: 12px;
  opacity: 0.7;
  margin-right: 10px;
  /* 用 anywhere 而非 break-all：后者会把中文和短单词也随意截断 */
  overflow-wrap: anywhere;
}
.roles-empty {
  padding: 24px 0;
  opacity: 0.6;
  font-size: 13px;
}
/* 84px 定宽左标签在 375px 下吃掉近三成宽度，手机改上下堆叠（等同表单 label-placement=top） */
@media (max-width: 768px) {
  .role-row {
    flex-direction: column;
    gap: 2px;
  }
  .role-k {
    flex: none;
  }
}
</style>
