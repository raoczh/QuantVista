export interface WorkspaceDestination {
  key: string
  path: string
  label: string
  icon: string
}

export interface WorkspaceGroup {
  key: string
  label: string
  admin?: boolean
  items: WorkspaceDestination[]
}

// 桌面侧栏、移动抽屉和页面位置提示共用同一份目录。
export const workspaceGroups: WorkspaceGroup[] = [
  { key: 'daily', label: '日常工作', items: [
    { key: 'home', path: '/', label: '今日概览', icon: 'home' },
    { key: 'today', path: '/today', label: '待办收件箱', icon: 'inbox' },
    { key: 'watchlist', path: '/watchlist', label: '自选股', icon: 'star' },
    { key: 'tasks', path: '/tasks', label: '任务中心', icon: 'activity' },
  ] },
  { key: 'research', label: '发现与研究', items: [
    { key: 'screener', path: '/screener', label: '策略选股', icon: 'filter' },
    { key: 'recommendations', path: '/recommendations', label: '推荐追踪', icon: 'compass' },
    { key: 'analysis', path: '/analysis', label: 'AI 分析', icon: 'sparkles' },
    { key: 'qa', path: '/qa', label: '个股问答', icon: 'message' },
    { key: 'compare', path: '/compare', label: '横向对比', icon: 'columns' },
  ] },
  { key: 'market', label: '市场动态', items: [
    { key: 'mood', path: '/mood', label: '盘面情绪', icon: 'activity' },
    { key: 'news', path: '/news', label: '市场快讯', icon: 'news' },
    { key: 'heatmap', path: '/heatmap', label: '行业热力图', icon: 'grid' },
    { key: 'etf', path: '/etf', label: '指数 ETF', icon: 'chart' },
  ] },
  { key: 'portfolio', label: '持仓与复盘', items: [
    { key: 'positions', path: '/positions', label: '持仓管理', icon: 'briefcase' },
    { key: 'portfolio-risk', path: '/portfolio-risk', label: '组合风险', icon: 'shield' },
    { key: 'alerts', path: '/alerts', label: '条件提醒', icon: 'bell' },
    { key: 'daily-report', path: '/daily-report', label: '收盘日报', icon: 'report' },
    { key: 'thesis', path: '/thesis', label: '投资逻辑卡', icon: 'layers' },
    { key: 'notes', path: '/notes', label: '投资笔记', icon: 'note' },
    { key: 'paper', path: '/paper', label: '模拟交易', icon: 'flask' },
  ] },
  { key: 'tools', label: '研究工具', items: [
    { key: 'backtest', path: '/backtest', label: '历史回测', icon: 'history' },
    { key: 'prompts', path: '/prompt-templates', label: '提示词模板', icon: 'sliders' },
  ] },
  { key: 'admin', label: '管理与评估', admin: true, items: [
    { key: 'admin', path: '/admin', label: '系统管理', icon: 'settings' },
    { key: 'admin-llm-calls', path: '/admin/llm-calls', label: '模型调用记录', icon: 'activity' },
    { key: 'admin-llm-roles', path: '/admin/llm-roles', label: '研究角色', icon: 'layers' },
    { key: 'admin-llm-experiments', path: '/admin/llm-experiments', label: '推荐影子实验', icon: 'flask' },
    { key: 'admin-selection-eval', path: '/admin/selection-eval', label: '选股配对评估', icon: 'columns' },
    { key: 'admin-factor-ic', path: '/admin/factor-ic', label: '因子有效性', icon: 'chart' },
    { key: 'admin-walk-forward', path: '/admin/walk-forward', label: '滚动验证', icon: 'history' },
    { key: 'admin-calibration', path: '/admin/calibration', label: '置信度校准', icon: 'compass' },
    { key: 'admin-joint-eval', path: '/admin/joint-eval', label: '联合评估', icon: 'grid' },
  ] },
]

export function workspaceLocation(routeName: string) {
  const key = routeName === 'board-detail' ? 'heatmap' : routeName
  for (const group of workspaceGroups) {
    const item = group.items.find(entry => entry.key === key)
    if (item) return { group: group.label, groupKey: group.key, item, activeKey: key }
  }
  if (routeName === 'stock-detail') return { group: '市场动态', groupKey: 'market', item: null, activeKey: '' }
  if (routeName === 'settings') return { group: '个人设置', groupKey: '', item: null, activeKey: 'settings' }
  return { group: '工作台', groupKey: '', item: null, activeKey: '' }
}
