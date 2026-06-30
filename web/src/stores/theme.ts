import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { darkTheme, type GlobalTheme } from 'naive-ui'
import { DEFAULT_THEME_KEY, findPreset, THEME_PRESETS } from '@/theme/presets'

const STORAGE_KEY = 'qv-theme'

// 全局主题 store：当前主题 + localStorage 持久化。
// 所有页面统一通过根部 n-config-provider 消费 naiveTheme / themeOverrides，
// 组件内若需取色用 useThemeVars()，禁止硬编码颜色（见 docs/ARCHITECTURE.md 主题规范）。
export const useThemeStore = defineStore('theme', () => {
  const saved = typeof localStorage !== 'undefined' ? localStorage.getItem(STORAGE_KEY) : null
  const currentKey = ref(saved && THEME_PRESETS.some((p) => p.key === saved) ? saved : DEFAULT_THEME_KEY)

  const preset = computed(() => findPreset(currentKey.value))
  const isDark = computed(() => preset.value.base === 'dark')
  const naiveTheme = computed<GlobalTheme | null>(() => (isDark.value ? darkTheme : null))
  const themeOverrides = computed(() => preset.value.overrides)

  function setTheme(key: string) {
    if (!THEME_PRESETS.some((p) => p.key === key)) return
    currentKey.value = key
    if (typeof localStorage !== 'undefined') localStorage.setItem(STORAGE_KEY, key)
  }

  return { currentKey, preset, isDark, naiveTheme, themeOverrides, presets: THEME_PRESETS, setTheme }
})
