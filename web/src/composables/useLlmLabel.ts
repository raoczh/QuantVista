import { ref } from 'vue'
import { listLLMConfigs } from '@/api/llm'

// 模块级缓存：多个页面共用一份「配置 id → 名称」映射，只请求一次。
const nameById = ref<Record<number, string>>({})
let loaded = false
let loading: Promise<void> | null = null

function ensureLoaded() {
  if (loaded || loading) return
  loading = listLLMConfigs()
    .then((cfgs) => {
      const m: Record<number, string> = {}
      for (const c of cfgs) m[c.id] = c.name
      nameById.value = m
      loaded = true
    })
    .catch(() => {
      loading = null // 失败允许下次进页面重试
    })
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
  ensureLoaded()
  function llmLabel(meta: LlmMetaLike | null | undefined): string {
    if (!meta) return ''
    const name = (meta.llm_config_id && nameById.value[meta.llm_config_id]) || meta.provider || ''
    const model = meta.model || ''
    if (name && model) return `${name} · ${model}`
    return model || name
  }
  return { llmLabel }
}
