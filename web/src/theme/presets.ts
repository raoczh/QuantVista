import type { GlobalThemeOverrides } from 'naive-ui'

// 一套主题预设：基色调（亮/暗）+ 主色及其衍生色 + 全套背景分层。
export interface ThemePreset {
  key: string
  label: string
  base: 'light' | 'dark'
  primary: string // 切换器上的色块预览，也是 Naive primaryColor
  overrides: GlobalThemeOverrides
}

// 主色四件套：primary / hover / pressed / suppl。
type Primaries = [string, string, string, string]

// 与 global.css 的 --qv-font-sans 保持一致，覆盖 Naive 默认字体栈。
const FONT_SANS =
  "-apple-system, BlinkMacSystemFont, 'Segoe UI', 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', 'Helvetica Neue', Arial, sans-serif"

// 6 套通用：控件圆角 8px（摆脱 Naive 默认 3px 的后台感）、加重强调字重。
const SHARED = {
  fontFamily: FONT_SANS,
  borderRadius: '8px',
  borderRadiusSmall: '6px',
  fontWeightStrong: '600',
}

function primaryOverrides([primary, hover, pressed, suppl]: Primaries) {
  return {
    primaryColor: primary,
    primaryColorHover: hover,
    primaryColorPressed: pressed,
    primaryColorSuppl: suppl,
  }
}

// 亮色主题：页面底色带一点主色倾向的浅灰，卡片保持纯白 → 背景与卡片分层。
function buildLight(p: Primaries, bodyColor: string): GlobalThemeOverrides {
  return {
    common: {
      ...SHARED,
      ...primaryOverrides(p),
      bodyColor,
      cardColor: '#ffffff',
      modalColor: '#ffffff',
      popoverColor: '#ffffff',
    },
  }
}

// 暗色主题：底色带品牌色调（不再是中性灰黑），卡片比底色浮起一档。
// Naive 暗色下 table/code 是固定灰黑，与带色调的卡片会打架，一并对齐。
function buildDark(
  p: Primaries,
  colors: { body: string; card: string; popover: string },
): GlobalThemeOverrides {
  return {
    common: {
      ...SHARED,
      ...primaryOverrides(p),
      bodyColor: colors.body,
      cardColor: colors.card,
      modalColor: colors.card,
      popoverColor: colors.popover,
      tableColor: colors.card,
      tableHeaderColor: colors.popover,
      codeColor: 'rgba(255, 255, 255, 0.08)',
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
    overrides: buildLight(['#2080f0', '#4098fc', '#1060c9', '#4098fc'], '#f3f5fa'),
  },
  {
    key: 'dark-blue',
    label: '深空蓝（深）',
    base: 'dark',
    primary: '#2080f0',
    overrides: buildDark(['#2080f0', '#4098fc', '#1060c9', '#4098fc'], {
      body: '#0d1322',
      card: '#141c30',
      popover: '#1a2440',
    }),
  },
  {
    key: 'dark-emerald',
    label: '极客绿（深）',
    base: 'dark',
    primary: '#18a058',
    overrides: buildDark(['#18a058', '#36ad6a', '#0c7a43', '#36ad6a'], {
      body: '#0c1511',
      card: '#122019',
      popover: '#182a21',
    }),
  },
  {
    key: 'light-violet',
    label: '典雅紫（浅）',
    base: 'light',
    primary: '#7c3aed',
    overrides: buildLight(['#7c3aed', '#9560f0', '#6428d6', '#9560f0'], '#f7f5fb'),
  },
  {
    key: 'dark-amber',
    label: '暖夜橙（深）',
    base: 'dark',
    primary: '#f0a020',
    overrides: buildDark(['#f0a020', '#fcb040', '#c97c10', '#fcb040'], {
      body: '#161009',
      card: '#211a10',
      popover: '#2b2216',
    }),
  },
  {
    key: 'light-rose',
    label: '樱桃红（浅）',
    base: 'light',
    primary: '#d03050',
    overrides: buildLight(['#d03050', '#de576d', '#ab1f3f', '#de576d'], '#faf5f6'),
  },
]

export const DEFAULT_THEME_KEY = 'dark-blue'

export function findPreset(key: string): ThemePreset {
  return THEME_PRESETS.find((p) => p.key === key) ?? THEME_PRESETS.find((p) => p.key === DEFAULT_THEME_KEY)!
}
