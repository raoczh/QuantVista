<script setup lang="ts">
import { ref, onBeforeUnmount, onMounted, computed } from 'vue'
import {
  NAlert,
  NButton,
  NInput,
  NSwitch,
  NSpin,
  NCollapse,
  NCollapseItem,
  NTag,
  useMessage,
} from 'naive-ui'
import {
  listPromptModules,
  listPromptTemplates,
  upsertPromptTemplate,
  deletePromptTemplate,
  type PromptModuleInfo,
  type PromptTemplate,
} from '@/api/prompt'
import { useUi } from '@/composables/useUi'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import { getSessionEpoch } from '@/api/token'

const message = useMessage()
const { vars } = useUi()
const styleVars = computed(() => ({ '--qv-divider': vars.value.dividerColor }))

const modules = ref<PromptModuleInfo[]>([])
const templates = ref<PromptTemplate[]>([])
const loading = ref(false)
const loadError = ref('')
const saving = ref('')
const savingAction = ref<'save' | 'reset'>('save')
const pageSession = getSessionEpoch()
let disposed = false
const pageActive = () => !disposed && getSessionEpoch() === pageSession
onBeforeUnmount(() => { disposed = true })

// 每模块的本地编辑态。
type PromptDraft = { content: string; enabled: boolean; id: number | null }
const drafts = ref<Record<string, PromptDraft>>({})

// 手动刷新只更新尚未编辑的字段；已有草稿与读取期间的新输入都保留。
function syncDrafts(previousTemplates: PromptTemplate[]) {
  const prev = drafts.value
  const map: Record<string, PromptDraft> = {}
  for (const m of modules.value) {
    const existing = prev[m.module]
    const baseline = previousTemplates.find((t) => t.module === m.module)
    const tpl = templates.value.find((t) => t.module === m.module)
    map[m.module] = {
      content: existing && existing.content !== (baseline?.content ?? '') ? existing.content : tpl?.content ?? '',
      enabled: existing && existing.enabled !== (baseline?.enabled ?? false) ? existing.enabled : tpl?.enabled ?? false,
      id: tpl?.id ?? null,
    }
  }
  drafts.value = map
}

async function load() {
  if (!pageActive() || loading.value || saving.value) return
  loading.value = true
  loadError.value = ''
  try {
    const [nextModules, nextTemplates] = await Promise.all([listPromptModules(), listPromptTemplates()])
    if (!pageActive()) return
    const previousTemplates = templates.value
    modules.value = nextModules
    templates.value = nextTemplates
    syncDrafts(previousTemplates)
  } catch (e) {
    if (pageActive()) loadError.value = e instanceof Error ? e.message : '提示词读取失败'
  } finally {
    loading.value = false
  }
}

// 写入响应已含完整模板。按提交快照回填，保留用户在等待期间继续编辑的字段。
function acceptTemplate(module: string, template: PromptTemplate | null, submitted: PromptDraft) {
  templates.value = templates.value.filter((t) => t.module !== module)
  if (template) templates.value.push(template)
  const draft = drafts.value[module]
  draft.id = template?.id ?? null
  if (draft.content === submitted.content) draft.content = template?.content ?? ''
  if (draft.enabled === submitted.enabled) draft.enabled = template?.enabled ?? false
}

async function save(m: PromptModuleInfo) {
  if (!pageActive() || loading.value || saving.value) return
  const d = drafts.value[m.module]
  if (!d?.content.trim()) {
    message.warning('模板内容不能为空（如需恢复默认请点「恢复默认」）')
    return
  }
  const submitted = { ...d }
  saving.value = m.module
  savingAction.value = 'save'
  try {
    const res = await upsertPromptTemplate({ module: m.module, content: submitted.content, enabled: submitted.enabled })
    if (!pageActive()) return
    acceptTemplate(m.module, res.template, submitted)
    message.success('已保存')
    // P0-6：占位符/内容 lint 诊断（不阻断保存，逐条提示）。
    for (const w of res.warnings ?? []) message.warning(w, { duration: 6000 })
  } catch (e) {
    if (pageActive()) message.error((e as Error).message)
  } finally {
    saving.value = ''
  }
}
async function reset(m: PromptModuleInfo) {
  if (!pageActive() || loading.value || saving.value) return
  const d = drafts.value[m.module]
  if (!d) return
  if (!d.id) {
    d.content = ''
    d.enabled = false
    return
  }
  const submitted = { ...d }
  saving.value = m.module
  savingAction.value = 'reset'
  try {
    await deletePromptTemplate(d.id)
    if (!pageActive()) return
    acceptTemplate(m.module, null, submitted)
    message.success('已恢复默认')
  } catch (e) {
    if (pageActive()) message.error((e as Error).message)
  } finally {
    saving.value = ''
  }
}
function useDefault(m: PromptModuleInfo) {
  drafts.value[m.module].content = m.default
}

// P0-6：模板行（含 content_hash/revision 归因信息）。
function tplOf(module: string) {
  return templates.value.find((t) => t.module === module)
}

function isDirty(module: string) {
  const draft = drafts.value[module]
  const template = tplOf(module)
  return draft && (draft.content !== (template?.content ?? '') || draft.enabled !== (template?.enabled ?? false))
}

onMounted(() => void load())
</script>

<template>
  <PageContainer title="提示词模板" subtitle="调整研究任务的关注重点；系统的证据核验与风险约束始终生效。">
    <div class="prompts" :style="styleVars">
      <SectionCard title="按模块自定义">
        <template #extra>
          <n-button size="tiny" quaternary :loading="loading" :disabled="!!saving" @click="load()">刷新</n-button>
        </template>
        <p class="tip">
          每个模块可写一段自定义任务指引（关注角度/语气/排序偏好），「启用」后该模块的 AI
          调用即时使用你的模板（无需重启）。自定义内容一律作为「任务段」注入：分析 5 模块替换分析维度指引，
          推荐/日报/问答/复核替换各自的角色定位段；各模块的准确性纪律、数据边界与 JSON 输出格式是
          <b>系统契约，由系统自动追加、不可被模板覆盖</b>（下方每个模块可查看其契约内容），
          模板里无需也不应再写输出格式要求。此前按「整段替换」写的旧模板已自动降级为任务段注入——
          若旧模板末尾自带 JSON schema 段，建议删除以免与系统契约重复。模板里可用占位符（如
          <code>{{ '\{\{symbol\}\}' }}</code>）注入运行时上下文，写错或未提供值的占位符会原样保留（保存时会提示诊断）。
          每次内容变化都会保存不可变快照并生成内容指纹（版本号 -custom.指纹 可在调用审计中归因到当时的模板原文）。
          留空并保存无效，如需恢复默认请点「恢复默认」。
        </p>
        <n-alert v-if="loadError" type="error" :bordered="false">{{ loadError }}</n-alert>
        <n-spin :show="loading && !modules.length">
          <n-collapse>
            <n-collapse-item v-for="m in modules" :key="m.module" :name="m.module">
              <template #header>
                <div class="mod-head">
                  <span class="mod-label">{{ m.label }}</span>
                  <n-tag
                    v-if="tplOf(m.module)"
                    size="tiny"
                    round
                    :bordered="false"
                    :type="tplOf(m.module)?.enabled ? 'success' : 'default'"
                    >{{ tplOf(m.module)?.enabled ? '自定义生效' : '自定义未启用' }}</n-tag
                  >
                  <n-tag v-else size="tiny" round :bordered="false">默认</n-tag>
                  <n-tag v-if="isDirty(m.module)" size="tiny" round :bordered="false" type="warning">未保存修改</n-tag>
                  <n-tag
                    v-if="tplOf(m.module)?.content_hash"
                    size="tiny"
                    round
                    :bordered="false"
                    class="ph-tag"
                    :title="'内容指纹（版本号 -custom.' + tplOf(m.module)!.content_hash!.slice(0, 8) + ' 可在调用审计中归因）'"
                    >r{{ tplOf(m.module)?.revision }} · {{ tplOf(m.module)!.content_hash!.slice(0, 8) }}</n-tag
                  >
                </div>
              </template>
              <div v-if="drafts[m.module]" class="mod-body">
                <div v-if="m.placeholders?.length" class="mod-placeholders">
                  可用占位符：
                  <n-tag v-for="p in m.placeholders" :key="p" size="tiny" :bordered="false" class="ph-tag">
                    {{ '{{' + p + '}' + '}' }}
                  </n-tag>
                </div>
                <div class="mod-default">
                  <div class="mod-default-title">默认任务段（参考——自定义替换的就是这一段）</div>
                  <pre class="mod-default-text">{{ m.default }}</pre>
                  <n-button size="tiny" quaternary @click="useDefault(m)">以默认为模板</n-button>
                </div>
                <div v-if="m.contract" class="mod-default">
                  <div class="mod-default-title">
                    系统契约（自动追加，不可覆盖——纪律与输出格式由系统保证，模板中无需再写）
                  </div>
                  <pre class="mod-default-text">{{ m.contract }}</pre>
                </div>
                <n-input
                  v-model:value="drafts[m.module].content"
                  type="textarea"
                  :autosize="{ minRows: 4, maxRows: 14 }"
                  placeholder="在此写自定义分析维度指引…"
                  maxlength="4000"
                />
                <div class="mod-actions">
                  <div class="mod-enable">
                    <span>启用</span>
                    <n-switch v-model:value="drafts[m.module].enabled" />
                  </div>
                  <div class="mod-btns">
                    <n-button size="small" quaternary :disabled="loading || !!saving" :loading="saving === m.module && savingAction === 'reset'" @click="reset(m)">恢复默认</n-button>
                    <n-button size="small" type="primary" :disabled="loading || !!saving" :loading="saving === m.module && savingAction === 'save'" @click="save(m)">保存</n-button>
                  </div>
                </div>
              </div>
            </n-collapse-item>
          </n-collapse>
        </n-spin>
      </SectionCard>
    </div>
  </PageContainer>
</template>

<style scoped>
.prompts {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.tip {
  font-size: 13px;
  opacity: 0.65;
  line-height: 1.6;
  margin: 0 0 14px;
}
.mod-head {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  min-width: 0;
}
.mod-label {
  font-size: 14px;
  font-weight: 600;
  min-width: 0;
  overflow-wrap: anywhere;
}
.mod-body {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 4px 0 8px;
}
.mod-placeholders {
  font-size: 12px;
  opacity: 0.75;
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
}
.ph-tag {
  font-family: monospace;
}
.mod-default {
  background: v-bind('vars.actionColor');
  border-radius: 8px;
  padding: 10px 12px;
}
.mod-default-title {
  font-size: 12px;
  opacity: 0.6;
  margin-bottom: 6px;
}
.mod-default-text {
  font-size: 12px;
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-word;
  margin: 0 0 8px;
  opacity: 0.8;
  font-family: inherit;
}
.mod-actions {
  display: flex;
  align-items: center;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: 10px;
}
.mod-enable {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
}
.mod-btns {
  display: flex;
  gap: 10px;
}
</style>
