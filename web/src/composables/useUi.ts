import { computed } from 'vue'
import { useThemeVars } from 'naive-ui'
import { storeToRefs } from 'pinia'
import { useThemeStore } from '@/stores/theme'

/** 把任意颜色（#hex 或 rgb/rgba）转成带透明度的 rgba。主题变量可能是任一种格式。 */
export function withAlpha(color: string, alpha: number): string {
  if (!color) return color
  if (color.startsWith('#')) {
    let hex = color.slice(1)
    if (hex.length === 3)
      hex = hex
        .split('')
        .map((c) => c + c)
        .join('')
    const r = parseInt(hex.slice(0, 2), 16)
    const g = parseInt(hex.slice(2, 4), 16)
    const b = parseInt(hex.slice(4, 6), 16)
    return `rgba(${r}, ${g}, ${b}, ${alpha})`
  }
  const m = color.match(/rgba?\(([^)]+)\)/)
  if (m) {
    const [r, g, b] = m[1].split(',').map((s) => s.trim())
    return `rgba(${r}, ${g}, ${b}, ${alpha})`
  }
  return color
}

/**
 * 全站统一 UI 工具：颜色全部取自 Naive 主题变量，自动兼容 6 套主题（明/暗）。
 * 语义色遵循 A 股习惯：涨 = errorColor(红)、跌 = successColor(绿)、平 = textColor3。
 * 见 docs/ARCHITECTURE.md §4.1「禁止硬编码颜色」硬约束。
 */
export function useUi() {
  const vars = useThemeVars()
  const { isDark } = storeToRefs(useThemeStore())

  const upColor = computed(() => vars.value.errorColor)
  const downColor = computed(() => vars.value.successColor)
  const flatColor = computed(() => vars.value.textColor3)

  /** 按涨跌正负返回文字色 */
  function pctColor(n: number): string {
    return n > 0 ? vars.value.errorColor : n < 0 ? vars.value.successColor : vars.value.textColor3
  }
  /** 按涨跌正负返回浅底色（pill 背景），暗色下加深一点保证可见 */
  function pctBg(n: number): string {
    const base =
      n > 0 ? vars.value.errorColor : n < 0 ? vars.value.successColor : vars.value.textColor3
    return withAlpha(base, isDark.value ? 0.18 : 0.1)
  }
  /** 主色叠加透明度，用于高亮/描边/发光 */
  function primaryAlpha(alpha: number): string {
    return withAlpha(vars.value.primaryColor, alpha)
  }

  return { vars, isDark, upColor, downColor, flatColor, pctColor, pctBg, primaryAlpha, withAlpha }
}
