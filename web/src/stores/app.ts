import { defineStore } from 'pinia'
import { ref } from 'vue'
import { getStatus, type StatusInfo } from '@/api/market'

// 后端健康状态 store，供首页与全局状态条复用。
export const useAppStore = defineStore('app', () => {
  const status = ref<StatusInfo | null>(null)
  const loading = ref(false)
  const error = ref('')

  async function refreshStatus() {
    loading.value = true
    error.value = ''
    try {
      status.value = await getStatus()
    } catch (e) {
      error.value = (e as Error).message
    } finally {
      loading.value = false
    }
  }

  return { status, loading, error, refreshStatus }
})
