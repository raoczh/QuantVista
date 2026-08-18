export interface TermDefinition {
  plain: string
  professional: string
  help: string
}

export const TERM_DICTIONARY = {
  alpha: {
    plain: '相对大盘多赚了多少',
    professional: 'Alpha',
    help: '回答这项投资扣除大盘影响后，多赚或少赚了多少。',
  },
  beta: {
    plain: '跟随大盘的敏感度',
    professional: 'Beta',
    help: '回答大盘涨跌时，这项投资通常会放大还是减弱波动。',
  },
  atr: {
    plain: '近期常见波动幅度',
    professional: 'ATR',
    help: '回答这只股票近期一天通常波动多大，常用于设置保护距离。',
  },
  ma: {
    plain: '一段时间的平均价格',
    professional: '移动平均线（MA）',
    help: '回答当前价格相对一段时间平均成本的位置，跌破或站上只是一项条件，不代表交易已经执行。',
  },
  rps: {
    plain: '相对市场强弱',
    professional: 'RPS',
    help: '回答这只股票近期走势在全市场中处于强还是弱。',
  },
  mfe: {
    plain: '持有期间最大浮盈',
    professional: 'MFE',
    help: '回答持有期间最多曾赚到多少，用来观察止盈空间。',
  },
  mae: {
    plain: '持有期间最大浮亏',
    professional: 'MAE',
    help: '回答持有期间最差曾亏到多少，用来观察承受的风险。',
  },
  twr: {
    plain: '剔除出入金影响的收益',
    professional: 'TWR',
    help: '回答不受充值和提现干扰时，投资本身赚了多少。',
  },
  sharpe: {
    plain: '每承担一份总波动得到的回报',
    professional: 'Sharpe',
    help: '回答承担全部波动后，获得的回报是否划算。',
  },
  sortino: {
    plain: '每承担一份下跌风险得到的回报',
    professional: 'Sortino',
    help: '回答只看不利波动时，获得的回报是否划算。',
  },
  ic: {
    plain: '因子预测方向的稳定程度',
    professional: 'IC',
    help: '回答某个因子与后续收益方向是否经常一致。',
  },
  icir: {
    plain: '因子预测稳定性',
    professional: 'ICIR',
    help: '回答因子的预测能力跨时间是否稳定，而不是偶尔有效。',
  },
  rankic: {
    plain: '因子排序有效性',
    professional: 'RankIC',
    help: '回答因子给出的强弱排序与后续收益排序是否一致。',
  },
  regime: {
    plain: '市场状态',
    professional: '市场状态（regime）',
    help: '回答当前市场更适合进攻、中性应对还是防守。',
  },
  quant_score: {
    plain: '规则评分',
    professional: '量化分',
    help: '回答程序按统一规则综合比较后，这只股票排在什么位置。',
  },
  confidence: {
    plain: '系统把握度',
    professional: '综合置信',
    help: '回答数据完整度、规则评分和证据核验合在一起后，系统有多大把握。',
  },
  ai_confidence: {
    plain: 'AI 自己的把握度',
    professional: 'AI 自评',
    help: '回答 AI 对自身结论有多大把握；它不等于程序核验后的可信度。',
  },
  as_of: {
    plain: '数据截止时间',
    professional: 'as_of（数据截止时间）',
    help: '回答这条结论实际使用了截止到什么时候的数据。',
  },
  partial: {
    plain: '数据不完整',
    professional: 'partial（数据不完整）',
    help: '回答本次结果是否缺少部分数据，缺失内容仍需谨慎对待。',
  },
  unknown: {
    plain: '暂时无法判断',
    professional: 'unknown（暂时无法判断）',
    help: '回答当前证据是否足够；显示此状态时不能当作没有风险。',
  },
  llm: {
    plain: 'AI 模型',
    professional: 'LLM',
    help: '回答是哪类生成式 AI 服务参与了文字分析。',
  },
  token: {
    plain: 'AI 用量',
    professional: 'Token',
    help: '回答一次 AI 请求处理了多少文字单位，主要用于了解用量。',
  },
} satisfies Record<string, TermDefinition>

export type TermKey = keyof typeof TERM_DICTIONARY
