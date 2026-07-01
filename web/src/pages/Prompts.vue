<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import {
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

const message = useMessage()
const { vars } = useUi()
const styleVars = computed(() => ({ '--qv-divider': vars.value.dividerColor }))

const modules = ref<PromptModuleInfo[]>([])
const templates = ref<PromptTemplate[]>([])
const loading = ref(false)

// 每模块的本地编辑态。
const drafts = ref<Record<string, { content: string; enabled: boolean; id: number | null }>>({})

function syncDrafts() {
  const map: Record<string, { content: string; enabled: boolean; id: number | null }> = {}
  for (const m of modules.value) {
    const tpl = templates.value.find((t) => t.module === m.module)
    map[m.module] = { content: tpl?.content ?? '', enabled: tpl?.enabled ?? false, id: tpl?.id ?? null }
  }
  drafts.value = map
}

async function load() {
  loading.value = true
  try {
    ;[modules.value, templates.value] = await Promise.all([listPromptModules(), listPromptTemplates()])
    syncDrafts()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}

const saving = ref('')
async function save(m: PromptModuleInfo) {
  const d = drafts.value[m.module]
  if (!d.content.trim()) {
    message.warning('模板内容不能为空（如需恢复默认请点「删除」）')
    return
  }
  saving.value = m.module
  try {
    await upsertPromptTemplate({ module: m.module, content: d.content, enabled: d.enabled })
    await load()
    message.success('已保存')
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    saving.value = ''
  }
}
async function reset(m: PromptModuleInfo) {
  const d = drafts.value[m.module]
  if (!d.id) {
    d.content = ''
    d.enabled = false
    return
  }
  try {
    await deletePromptTemplate(d.id)
    await load()
    message.success('已恢复默认')
  } catch (e) {
    message.error((e as Error).message)
  }
}
function useDefault(m: PromptModuleInfo) {
  drafts.value[m.module].content = m.default
}

onMounted(load)
</script>

<template>
  <PageContainer title="提示词模板" subtitle="自定义各分析模块的系统提示 · 启用后覆盖默认分析维度指引">
    <div class="prompts" :style="styleVars">
      <SectionCard title="按模块自定义">
        <template #extra>
          <n-button size="tiny" quaternary :loading="loading" @click="load">刷新</n-button>
        </template>
        <p class="tip">
          每个分析模块可写一段自定义指引，替换系统默认的分析维度说明；「启用」后该模块的 AI 分析将使用你的模板。
          通用的合规身份与 JSON 输出规范仍由系统保证。留空并保存无效，如需恢复默认请点「恢复默认」。
        </p>
        <n-spin :show="loading && !modules.length">
          <n-collapse>
            <n-collapse-item v-for="m in modules" :key="m.module" :name="m.module">
              <template #header>
                <div class="mod-head">
                  <span class="mod-label">{{ m.label }}</span>
                  <n-tag
                    v-if="drafts[m.module]?.id"
                    size="tiny"
                    round
                    :bordered="false"
                    :type="drafts[m.module]?.enabled ? 'success' : 'default'"
                    >{{ drafts[m.module]?.enabled ? '自定义生效' : '自定义未启用' }}</n-tag
                  >
                  <n-tag v-else size="tiny" round :bordered="false">默认</n-tag>
                </div>
              </template>
              <div v-if="drafts[m.module]" class="mod-body">
                <div class="mod-default">
                  <div class="mod-default-title">默认指引（参考）</div>
                  <pre class="mod-default-text">{{ m.default }}</pre>
                  <n-button size="tiny" quaternary @click="useDefault(m)">以默认为模板</n-button>
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
                    <n-button size="small" quaternary @click="reset(m)">恢复默认</n-button>
                    <n-button size="small" type="primary" :loading="saving === m.module" @click="save(m)">保存</n-button>
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
}
.mod-label {
  font-size: 14px;
  font-weight: 600;
}
.mod-body {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 4px 0 8px;
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
