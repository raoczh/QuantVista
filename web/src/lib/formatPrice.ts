// 单价按保存精度最多展示四位，保留至少两位；ETF 等标的不能截掉第三位有效价格。
const priceFormatter = new Intl.NumberFormat('zh-CN', {
  minimumFractionDigits: 2,
  maximumFractionDigits: 4,
  useGrouping: false,
})

export function formatPrice(value: number | null | undefined): string {
  return value == null || !Number.isFinite(value) ? '—' : priceFormatter.format(value)
}
