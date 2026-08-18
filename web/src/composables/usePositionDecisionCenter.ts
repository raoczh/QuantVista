import type { Position, PositionExitAssessment, PositionExitLevel } from '@/api/position'

export interface PositionDecisionRow {
  position: Position
  assessment: PositionExitAssessment
  focused: boolean
  historical: boolean
}

const levelRank: Record<PositionExitLevel, number> = {
  urgent: 5,
  review: 4,
  watch: 3,
  normal: 2,
  unknown: 1,
}

export const positionExitLevelLabel: Record<PositionExitLevel, string> = {
  urgent: '紧急处理',
  review: '需要复核',
  watch: '继续观察',
  normal: '暂时正常',
  unknown: '暂时无法判断',
}

export function buildPositionDecisionRows(
  positions: Position[],
  focusedPositionID: number | null,
  focusedAssessment: PositionExitAssessment | null,
): PositionDecisionRow[] {
  const rows = positions
    .filter(
      (position) =>
        position.status === 'holding' &&
        position.exit_assessment &&
        (position.exit_assessment.level === 'urgent' || position.exit_assessment.level === 'review'),
    )
    .map((position) => ({
      position,
      assessment:
        focusedAssessment && focusedAssessment.position_id === position.id
          ? focusedAssessment
          : position.exit_assessment!,
      focused: focusedPositionID === position.id,
      historical:
        !!focusedAssessment &&
        focusedAssessment.position_id === position.id &&
        focusedAssessment.id !== position.exit_assessment!.id,
    }))

  const focusedPosition = positions.find((position) => position.id === focusedPositionID)
  if (
    focusedPosition &&
    focusedAssessment &&
    !rows.some((row) => row.position.id === focusedPosition.id)
  ) {
    rows.push({
      position: focusedPosition,
      assessment: focusedAssessment,
      focused: true,
      historical: focusedAssessment.id !== focusedPosition.exit_assessment?.id,
    })
  }

  return rows.sort((left, right) => {
    if (left.focused !== right.focused) return left.focused ? -1 : 1
    const levelDiff = levelRank[right.assessment.level] - levelRank[left.assessment.level]
    if (levelDiff) return levelDiff
    return new Date(right.assessment.evaluated_at).getTime() - new Date(left.assessment.evaluated_at).getTime()
  })
}

export function latestDecisionTime(rows: PositionDecisionRow[]) {
  return rows.reduce((latest, row) => {
    const value = row.assessment.evaluated_at || ''
    return value > latest ? value : latest
  }, '')
}

export function positionDataStatusText(assessment: PositionExitAssessment) {
  if (assessment.data_status === 'ready') return '数据完整'
  if (assessment.data_status === 'partial') return '数据不完整'
  return '数据暂时不足'
}
