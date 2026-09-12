import { ref, watch } from 'vue'
import { listLLMConfigs } from '@/api/llm'
import { getSessionEpoch } from '@/api/token'
import { useAuthStore } from '@/stores/auth'

// 同一会话共用名称映射和在途请求；进入页面时刷新，反映设置页的重命名或新增。
const nameById = ref<Record<number, string>>({})
let cacheSession = -1
let loading: Promise<void> | null = null

function ensureLoaded() {
  const epoch = getSessionEpoch()
  if (cacheSession !== epoch) {
    nameById.value = {}
    cacheSession = epoch
    loading = null
  }
  if (loading) return
  const promise = listLLMConfigs()
    .then((cfgs) => {
      if (cacheSession !== epoch || getSessionEpoch() !== epoch) return
      const m: Record<number, string> = {}
      for (const c of cfgs) m[c.id] = c.name
      nameById.value = m
    })
    .catch(() => { /* 名称是可选展示，失败沿用 provider，下一次进页面重试。 */ })
    .finally(() => {
      if (loading === promise) loading = null
    })
  loading = promise
}

export interface LlmMetaLike {
  llm_config_id?: number
  provider?: string
  model?: string
}

/**
 * AI 记录的 LLM 展示标签：「配置名 · 模型」。配置已删或用的是管理员回退配置
 * （不在自己的配置清单里）时退回 provider；旧记录两者皆缺时返回空串，调用方 v-if 兜底。
 */
export function useLlmLabel() {
  const auth = useAuthStore()
  watch(() => auth.user?.id || 0, (userID) => {
    if (!userID) {
      nameById.value = {}
      cacheSession = -1
      loading = null
      return
    }
    ensureLoaded()
  }, { immediate: true, flush: 'sync' })
  function llmLabel(meta: LlmMetaLike | null | undefined): string {
    if (!meta) return ''
    const names = cacheSession === getSessionEpoch() ? nameById.value : {}
    const name = (meta.llm_config_id && names[meta.llm_config_id]) || meta.provider || ''
    const model = meta.model || ''
    if (name && model) return `${name} · ${model}`
    return model || name
  }
  return { llmLabel }
}
