import { request } from './client'
import type { PortfolioExposure } from './position'

export type PortfolioKind = 'real' | 'paper'
export interface PortfolioAccount { id:number; user_id:number; name:string; kind:PortfolioKind; currency:string; status:'active'|'archived'; is_default:boolean; created_at:string; updated_at:string }
export interface RiskMetric { status:'available'|'partial'|'unavailable'; value?:number; reason?:string; sample_count?:number }
export interface HoldingWeight { symbol:string; market:string; name:string; industry?:string; quantity:number; price?:number; value?:number; weight_pct?:number; status:string; reason?:string; plan_stop_loss?:number; valuation_known:boolean }
export interface PortfolioOverview { account:PortfolioAccount; as_of:string; total_assets:RiskMetric; market_value:number; cash:RiskMetric; holding_count:number; priced_count:number; coverage_pct:number; top_n_weight_pct:number; holdings:HoldingWeight[]; exposure?:PortfolioExposure; partial_reasons:string[]; data_version:string }
export interface EquityPoint { trade_date:string; assets:number; cash_flow?:number; return?:number; drawdown_pct?:number; partial:boolean }
export interface DrawdownResult { metric:RiskMetric; peak_date?:string; trough_date?:string; recovery_date?:string }
export interface CorrelationCell { status:string; value?:number; sample_count:number; reason?:string }
export interface CorrelationMatrix { symbols:string[]; cells:CorrelationCell[][]; window_days:number; as_of:string; data_version:string }
export interface RiskContribution { predicted_volatility_pct:RiskMetric; items:{symbol:string;weight_pct:number;marginal_volatility_pct?:number;component_volatility_pct?:number;risk_contribution_pct?:number;status:string;reason?:string}[]; sample_count:number; window_days:number; as_of:string; data_version:string }
export interface PortfolioRisk { account_id:number; as_of:string; window_days:number; parameter_hash:string; parameters:{annualization:number;risk_free_rate_pct:number;window_days:number;benchmark_code:string;as_of:string;version:string}; twr_pct:RiskMetric; annualized_volatility_pct:RiskMetric; downside_volatility_pct:RiskMetric; sharpe:RiskMetric; sortino:RiskMetric; beta:RiskMetric; alpha_pct:RiskMetric; max_drawdown:DrawdownResult; curve:EquityPoint[]; correlation:CorrelationMatrix; risk_contribution:RiskContribution; partial_count:number; unknown_reasons:string[]; data_version:string }
export interface CashFlow { id:number; user_id:number; account_id:number; type:string; amount:number; trade_date:string; note:string; idempotency_key:string; reversal_of_id?:number; created_at:string }
export interface StressScenario { type:'market'|'industry'|'symbol'|'plan_stop_loss'; shock_pct:number; symbol?:string; industry?:string }
export interface StressResult { scenario:StressScenario; estimated_loss_amount:number; estimated_loss_pct:number; contributions:{symbol:string;name:string;loss_amount:number;loss_pct:number}[]; unknown:string[]; base_value:number; generated_at:string; read_only:boolean }
export interface TargetItem { type:'symbol'|'industry'; key:string; target_weight_pct:number; min_weight_pct:number; max_weight_pct:number; enabled:boolean }
export interface TargetRevision { id:number; account_id:number; revision:number; content_hash:string; created_at:string }
export interface RebalanceItem { type:string; key:string; name:string; current_weight_pct:number; target_weight_pct:number; min_weight_pct:number; max_weight_pct:number; deviation_pct:number; amount_change:number; quantity_change:number; estimated_fee:number; estimated_tax:number; status:string; reason?:string }
export interface RebalanceDraft { account_id:number; revision:number; revision_hash:string; as_of:string; total_assets:RiskMetric; items:RebalanceItem[]; read_only:boolean; note:string }

export const listPortfolios=()=>request<PortfolioAccount[]>({url:'/portfolios'})
export const createPortfolio=(data:{name:string;kind:PortfolioKind;currency:string})=>request<PortfolioAccount>({url:'/portfolios',method:'post',data})
export const updatePortfolio=(id:number,name:string)=>request<PortfolioAccount>({url:`/portfolios/${id}`,method:'put',data:{name}})
export const archivePortfolio=(id:number)=>request<PortfolioAccount>({url:`/portfolios/${id}/archive`,method:'post'})
export const deletePortfolio=(id:number)=>request<void>({url:`/portfolios/${id}`,method:'delete'})
export const setDefaultPortfolio=(id:number)=>request<PortfolioAccount>({url:`/portfolios/${id}/default`,method:'post'})
export const getPortfolioOverview=(id:number)=>request<PortfolioOverview>({url:`/portfolios/${id}/overview`})
export const getPortfolioRisk=(id:number,params:Record<string,unknown>)=>request<PortfolioRisk>({url:`/portfolios/${id}/risk`,params})
export const listCashFlows=(id:number)=>request<CashFlow[]>({url:`/portfolios/${id}/cash-flows`})
export const addCashFlow=(id:number,data:{type:string;amount:number;trade_date:string;note:string;idempotency_key:string})=>request<CashFlow>({url:`/portfolios/${id}/cash-flows`,method:'post',data})
export const reverseCashFlow=(id:number,flowId:number,data:{idempotency_key:string;note:string})=>request<CashFlow>({url:`/portfolios/${id}/cash-flows/${flowId}/reverse`,method:'post',data})
export const runStress=(id:number,data:StressScenario)=>request<StressResult>({url:`/portfolios/${id}/stress-tests`,method:'post',data})
export const getTargets=(id:number,revision?:number)=>request<{revision:TargetRevision|null;items:TargetItem[]}>({url:`/portfolios/${id}/targets`,params:{revision}})
export const saveTargets=(id:number,items:TargetItem[])=>request<TargetRevision>({url:`/portfolios/${id}/targets`,method:'post',data:{items}})
export const getRebalance=(id:number,revision?:number)=>request<RebalanceDraft>({url:`/portfolios/${id}/rebalance`,params:{revision}})
