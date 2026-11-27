import { describe, expect, it } from 'vitest'
import { changePercent, emptyMetrics, sumMetrics } from '../metrics'

describe('shaping aggregates', () => {
  it('uses the ratio of totals instead of averaging entity percentages', () => {
    const total = sumMetrics([
      {...emptyMetrics(), requests: 20, comparable_requests: 10, before_cost: 100, after_cost: 200, before_tokens: 1000, after_tokens: 800},
      {...emptyMetrics(), requests: 10, comparable_requests: 10, before_cost: 900, after_cost: 900, before_tokens: 3000, after_tokens: 3200}
    ])
    expect(total).toEqual({requests:30,comparable_requests:20,before_cost:1000,after_cost:1100,before_tokens:4000,after_tokens:4000})
    expect(changePercent(total.before_cost,total.after_cost)).toBe(10)
    expect(changePercent(total.before_tokens,total.after_tokens)).toBe(0)
  })
  it('handles zero baselines and shrinkage', () => {
    expect(changePercent(0,0)).toBe(0)
    expect(changePercent(0,5)).toBeNull()
    expect(changePercent(100,50)).toBe(-50)
  })
})
