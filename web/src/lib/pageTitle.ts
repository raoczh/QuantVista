// 标签页标题统一拼装：页面名（路由切换更新）+ 大盘行情（盘中轮询更新）。
// 两个来源各自 set，这里负责合成，避免 router 与轮询互相覆盖。

let pageTitle = ''
let marketTitle = ''

function apply() {
  const app = pageTitle ? `QuantVista · ${pageTitle}` : 'QuantVista'
  document.title = marketTitle ? `${marketTitle} | ${app}` : app
}

export function setPageTitle(title: string) {
  pageTitle = title
  apply()
}

/** 传空串清除行情段（如接口失败时） */
export function setMarketTitle(title: string) {
  marketTitle = title
  apply()
}
