import { computed, ref, watch } from 'vue'
import { useAuthStore } from '@/stores/auth'

export type DisplayMode = 'plain' | 'professional'

const mode = ref<DisplayMode>('plain')
let activeStorageKey = ''

export function displayModeStorageKey(userID: number): string {
  return `qv-display-mode:${userID > 0 ? userID : 'guest'}`
}
function readMode(key: string): DisplayMode {
  return localStorage.getItem(key) === 'professional' ? 'professional' : 'plain'
}

export function useDisplayMode() {
  const auth = useAuthStore()

  watch(
    () => auth.user?.id || 0,
    (userID) => {
      activeStorageKey = displayModeStorageKey(userID)
      mode.value = readMode(activeStorageKey)
    },
    { immediate: true },
  )

  function setMode(value: DisplayMode) {
    mode.value = value
    const key = activeStorageKey || displayModeStorageKey(auth.user?.id || 0)
    localStorage.setItem(key, value)
  }

  function label(plain: string, professional: string): string {
    return mode.value === 'plain' ? plain : professional
  }

  return {
    displayMode: computed(() => mode.value),
    isPlain: computed(() => mode.value === 'plain'),
    setMode,
    label,
  }
}
