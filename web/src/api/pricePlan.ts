import type { ExitPlanSeed } from './position'

export interface ResearchPriceContext {
  strategy_key: string
  strategy_name: string
  strategy_revision_id?: number
  profile: string
  intent: string
  horizon: 'short_term' | 'long_term'
}

export interface ResearchPricePlan {
  version: string
  input_hash: string
  context: ResearchPriceContext
  quote_as_of: string
  bars_as_of: string
  reference_price: number
  status: 'ready' | 'wait' | 'unavailable'
  buy_low: number
  buy_high: number
  entry_anchor: number
  entry_atr: number
  horizon_days: number
  entry_valid_days: number
  reasons: string[]
  setup_reasons?: string[]
  evidence: string[]
  exit?: ExitPlanSeed
}
