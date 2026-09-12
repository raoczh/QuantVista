import { defineStore } from 'pinia'
import { ref } from 'vue'
import { getStatus, type StatusInfo } from '@/api/market'

// 后端健康状态 store，供首页与全局状态条复用。
export const useAppStore = defineStore('app', () => {
  const status = ref<StatusInfo | null>(null)
  const loading = ref(false)
  const error = ref('')
  let requestSequence = 0

  async function refreshStatus() {
    const sequence = ++requestSequence
    loading.value = true
    error.value = ''
    try {
      const value = await getStatus()
      if (sequence === requestSequence) status.value = value
    } catch (e) {
      if (sequence === requestSequence) error.value = (e as Error).message
    } finally {
      if (sequence === requestSequence) loading.value = false
    }
  }

  return { status, loading, error, refreshStatus }
})
