import { ref } from 'vue'
import { isNavigationFailure, useRouter, type RouteLocationRaw } from 'vue-router'
import { useMessage } from 'naive-ui'
import { listWatchlists, addItem } from '@/api/watchlist'
import { useAuthStore } from '@/stores/auth'
import { recordRecentStock } from '@/composables/useRecentStocks'

export interface StockRef {
  symbol: string
  market: string
  name: string
}

export type StockActionKey =
  | 'detail'
  | 'watchlist'
  | 'position'
  | 'position-decision'
  | 'alert'
  | 'analysis'
  | 'recommendation-review'
  | 'compare'
  | 'note'

export interface StockActionContext {
  inWatchlist?: boolean
  hasPosition?: boolean
  positionID?: number
  recommendationID?: number
}

let stockActionSequence = 0

/**
 * 个股快捷动作：统一携带 symbol/market/name 直达详情、持仓、提醒、分析、对比与笔记。
 * 必须在 setup（且 n-message-provider 内）调用。
 */
export function useStockActions(onNavigate?: () => void) {
  const router = useRouter()
  const message = useMessage()
  const auth = useAuthStore()
  const adding = ref(false)

  function remember(userID: number, s: StockRef) {
    if (userID > 0 && auth.user?.id === userID) recordRecentStock(userID, s)
  }
  function stockQuery(s: StockRef, extra: Record<string, string> = {}) {
    return {
      symbol: s.symbol,
      market: s.market || 'cn',
      name: s.name || '',
      ...extra,
      _stock_action: `${Date.now().toString(36)}-${++stockActionSequence}`,
    }
  }
  async function go(to: RouteLocationRaw, s: StockRef) {
    const userID = auth.user?.id || 0
    try {
      const failure = await router.push(to)
      if (failure && isNavigationFailure(failure)) {
        message.warning('未能打开目标页面，请重试')
        return false
      }
      remember(userID, s)
      onNavigate?.()
      return true
    } catch (e) {
      message.error((e as Error).message)
      return false
    }
  }
  function goAnalysis(s: StockRef) {
    return go({ name: 'analysis', query: stockQuery(s, { module: 'stock' }) }, s)
  }
  function goRecommendationReview(s: StockRef, recommendationID: number, context = '') {
    return go(
      {
        name: 'analysis',
        query: stockQuery(s, {
          module: 'stock',
          recommendation_id: String(recommendationID),
          review_context: 'recommendation',
          ...(context.trim() ? { recommendation_context: context.trim().slice(0, 320) } : {}),
        }),
      },
      s,
    )
  }
  function goQa(s: StockRef) {
    return go({ name: 'qa', query: stockQuery(s) }, s)
  }
  function goCompare(s: StockRef) {
    return go({ name: 'compare', query: stockQuery(s, { symbols: s.symbol }) }, s)
  }
  function goAlert(s: StockRef) {
    return go({ name: 'alerts', query: stockQuery(s, { add: '1' }) }, s)
  }
  function goThesis(s: StockRef) {
    return go({ name: 'thesis', query: stockQuery(s, { add: '1' }) }, s)
  }
  function goWatchlist(s: StockRef) {
    return go({ name: 'watchlist', query: stockQuery(s) }, s)
  }
  function goPosition(s: StockRef, hasPosition?: boolean) {
    return go({ name: 'positions', query: stockQuery(s, hasPosition === false ? { add: '1' } : {}) }, s)
  }
  function goPositionFromRecommendation(s: StockRef, recommendationID: number, quantity?: number) {
    if (!Number.isSafeInteger(recommendationID) || recommendationID <= 0) {
      message.error('缺少准确的推荐 ID，无法保留建仓血缘')
      return Promise.resolve(false)
    }
    return go(
      {
        name: 'positions',
        query: stockQuery(s, {
          add: '1',
          rec_id: String(recommendationID),
          ...(quantity && quantity > 0 ? { quantity: String(quantity) } : {}),
        }),
      },
      s,
    )
  }
  function goPositionDecision(s: StockRef, positionID: number) {
    if (!Number.isSafeInteger(positionID) || positionID <= 0) {
      message.error('缺少准确的持仓 ID，无法进入卖出决策')
      return Promise.resolve(false)
    }
    return go(
      {
        name: 'positions',
        query: stockQuery(s, {
          tab: 'needs_action',
          position_id: String(positionID),
        }),
      },
      s,
    )
  }
  function goNote(s: StockRef) {
    return go({ name: 'notes', query: stockQuery(s, { add: '1' }) }, s)
  }
  /** 个股详情页（行情/K线/估值/评分一页看全） */
  function goDetail(s: StockRef) {
    return go(
      {
        name: 'stock-detail',
        params: { market: s.market || 'cn', symbol: s.symbol },
        query: stockQuery(s),
      },
      s,
    )
  }

  /** 加入第一个自选分组（自用默认习惯，免选组打断） */
  async function addToWatchlist(s: StockRef) {
    const userID = auth.user?.id || 0
    adding.value = true
    try {
      const groups = await listWatchlists()
      if (!groups.length) {
        message.warning('还没有自选分组，请先到自选页创建一个')
        return false
      }
      await addItem(groups[0].id, { symbol: s.symbol, market: s.market, name: s.name })
      remember(userID, s)
      message.success(`已将 ${s.name || s.symbol} 加入「${groups[0].name}」`)
      return true
    } catch (e) {
      message.error((e as Error).message)
      return false
    } finally {
      adding.value = false
    }
  }

  function runStockAction(action: StockActionKey, s: StockRef, context: StockActionContext = {}) {
    switch (action) {
      case 'detail':
        return goDetail(s)
      case 'watchlist':
        return context.inWatchlist === false ? addToWatchlist(s) : goWatchlist(s)
      case 'position':
        return goPosition(s, context.hasPosition)
      case 'position-decision':
        return goPositionDecision(s, context.positionID || 0)
      case 'alert':
        return goAlert(s)
      case 'analysis':
        return goAnalysis(s)
      case 'recommendation-review':
        return goRecommendationReview(s, context.recommendationID || 0)
      case 'compare':
        return goCompare(s)
      case 'note':
        return goNote(s)
    }
  }

  return {
    adding,
    goAnalysis,
    goQa,
    goCompare,
    goAlert,
    goThesis,
    goWatchlist,
    goPosition,
    goPositionFromRecommendation,
    goPositionDecision,
    goNote,
    goDetail,
    goRecommendationReview,
    addToWatchlist,
    runStockAction,
  }
}
