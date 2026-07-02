import type { GlobalThemeOverrides } from 'naive-ui'

// 一套主题预设：基色调（亮/暗）+ 主色及其衍生色。
export interface ThemePreset {
  key: string
  label: string
  base: 'light' | 'dark'
  primary: string // 切换器上的色块预览，也是 Naive primaryColor
  overrides: GlobalThemeOverrides
}

// 由主色四件套生成 Naive 的 common 覆盖。
function build(primary: string, hover: string, pressed: string, suppl: string): GlobalThemeOverrides {
  return {
    common: {
      primaryColor: primary,
      primaryColorHover: hover,
      primaryColorPressed: pressed,
      primaryColorSuppl: suppl,
    },
  }
}

// 6 套主流主题：亮 3 / 暗 3，主色覆盖 蓝（亮/暗）/翠绿/紫/琥珀/玫红。
// 衍生色尽量沿用 Naive 官方调色，自定义色按相近规律取 hover 提亮 / pressed 压暗。
export const THEME_PRESETS: ThemePreset[] = [
  {
    key: 'light-blue',
    label: '极简蓝（浅）',
    base: 'light',
    primary: '#2080f0',
    overrides: build('#2080f0', '#4098fc', '#1060c9', '#4098fc'),
  },
  {
    key: 'dark-blue',
    label: '深空蓝（深）',
    base: 'dark',
    primary: '#2080f0',
    overrides: build('#2080f0', '#4098fc', '#1060c9', '#4098fc'),
  },
  {
    key: 'dark-emerald',
    label: '极客绿（深）',
    base: 'dark',
    primary: '#18a058',
    overrides: build('#18a058', '#36ad6a', '#0c7a43', '#36ad6a'),
  },
  {
    key: 'light-violet',
    label: '典雅紫（浅）',
    base: 'light',
    primary: '#7c3aed',
    overrides: build('#7c3aed', '#9560f0', '#6428d6', '#9560f0'),
  },
  {
    key: 'dark-amber',
    label: '暖夜橙（深）',
    base: 'dark',
    primary: '#f0a020',
    overrides: build('#f0a020', '#fcb040', '#c97c10', '#fcb040'),
  },
  {
    key: 'light-rose',
    label: '樱桃红（浅）',
    base: 'light',
    primary: '#d03050',
    overrides: build('#d03050', '#de576d', '#ab1f3f', '#de576d'),
  },
]

export const DEFAULT_THEME_KEY = 'dark-blue'

export function findPreset(key: string): ThemePreset {
  return THEME_PRESETS.find((p) => p.key === key) ?? THEME_PRESETS.find((p) => p.key === DEFAULT_THEME_KEY)!
}
