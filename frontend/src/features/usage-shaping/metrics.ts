import type { UsageShapingMetrics } from '@/api/admin/usage'

export const emptyMetrics = (): UsageShapingMetrics => ({ requests: 0, comparable_requests: 0, before_tokens: 0, after_tokens: 0, before_cost: 0, after_cost: 0 })
export const metricKeys = ['requests', 'comparable_requests', 'before_tokens', 'after_tokens', 'before_cost', 'after_cost'] as const
export function sumMetrics(rows: UsageShapingMetrics[]): UsageShapingMetrics {
  const sum = emptyMetrics()
  for (const row of rows) for (const key of metricKeys) sum[key] += row[key]
  return sum
}
export function changePercent(before: number, after: number): number | null {
  if (before === 0) return after === 0 ? 0 : null
  return (after - before) / before * 100
}
export const entityKey = (id: number | null): string => id == null ? 'ungrouped' : String(id)
