import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { useMessage } from 'naive-ui'
import { listWatchlists, addItem } from '@/api/watchlist'

export interface StockRef {
  symbol: string
  market: string
  name: string
}

/**
 * 个股快捷动作：从任意位置一键直达 AI 分析 / 问答 / 对比 / 设提醒，或直接加自选。
 * 目标页均支持 query 预填（Analysis/Qa/Alerts 原生支持，Compare 走 ?symbols=）。
 * 必须在 setup（且 n-message-provider 内）调用。
 */
export function useStockActions(onNavigate?: () => void) {
  const router = useRouter()
  const message = useMessage()
  const adding = ref(false)

  function go(name: string, query: Record<string, string>) {
    onNavigate?.()
    router.push({ name, query })
  }
  function goAnalysis(s: StockRef) {
    go('analysis', { symbol: s.symbol, market: s.market })
  }
  function goQa(s: StockRef) {
    go('qa', { symbol: s.symbol, market: s.market })
  }
  function goCompare(s: StockRef) {
    go('compare', { symbols: s.symbol })
  }
  function goAlert(s: StockRef) {
    go('alerts', { add: '1', symbol: s.symbol, market: s.market, name: s.name })
  }

  /** 加入第一个自选分组（自用默认习惯，免选组打断） */
  async function addToWatchlist(s: StockRef) {
    adding.value = true
    try {
      const groups = await listWatchlists()
      if (!groups.length) {
        message.warning('还没有自选分组，请先到自选页创建一个')
        return
      }
      await addItem(groups[0].id, { symbol: s.symbol, market: s.market, name: s.name })
      message.success(`已将 ${s.name || s.symbol} 加入「${groups[0].name}」`)
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      adding.value = false
    }
  }

  return { adding, goAnalysis, goQa, goCompare, goAlert, addToWatchlist }
}
