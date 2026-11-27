import { describe, expect, it, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { defineComponent, ref } from 'vue'

const mocks = vi.hoisted(() => ({ getShapingStats: vi.fn() }))
vi.mock('@/api/admin/usage', () => mocks)
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key, locale: ref('en-US') }) }))
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: defineComponent({ template: '<main><slot /></main>' }) }))
vi.mock('@/components/common/DateRangePicker.vue', () => ({ default: defineComponent({ template: '<div />' }) }))
vi.mock('@/components/common/Pagination.vue', () => ({ default: defineComponent({ template: '<div />' }) }))
vi.mock('@/components/common/LoadingSpinner.vue', () => ({ default: defineComponent({ template: '<div />' }) }))
vi.mock('vue-chartjs', () => ({
  Line: defineComponent({ name: 'ShapingLine', props: ['data','options'], template: '<div class="line-chart" />' }),
  Bar: defineComponent({ name: 'Bar', props: ['data','options'], template: '<div class="bar-chart" />' })
}))
import UsageShapingStatsView from '../UsageShapingStatsView.vue'

const point = (id: number, name: string, before: number, after: number) => ({id,name,requests:10,comparable_requests:8,before_cost:before,after_cost:after,before_tokens:1000,after_tokens:1200})
beforeEach(() => {
  const a=point(1,'Alpha',1,2),b=point(2,'Beta',3,2)
  mocks.getShapingStats.mockResolvedValue({summary:{},breakdown:[a,b],series:[{...a,bucket:'2026-11-27T00:00:00Z'},{...b,bucket:'2026-11-27T00:00:00Z'}],buckets:['2026-11-27T00:00:00Z'],granularity:'day'})
})
describe('Usage shaping aggregate page', () => {
  it('keeps account, group, chart and text filters in sync', async () => {
    const wrapper=mount(UsageShapingStatsView)
    await flushPromises()
    expect(wrapper.findAll('tbody tr')).toHaveLength(2)
    expect(wrapper.find('tfoot').text()).toContain('$4.0000')
    const line=wrapper.findComponent({name:'ShapingLine'})
    expect(line.props('data').datasets[0].data).toEqual([4])
    expect(line.props('data').datasets[1].data).toEqual([4])
    await wrapper.find('input[type="search"]').setValue('Alpha')
    expect(wrapper.findAll('tbody tr')).toHaveLength(1)
    expect(line.props('data').datasets[0].data).toEqual([1])
    expect(line.props('data').datasets[1].data).toEqual([2])
    expect(wrapper.find('tfoot').text()).toContain('+100.0%')
    const group=wrapper.findAll('button').find(b=>b.text()==='admin.usageShaping.group')
    await group!.trigger('click');await flushPromises()
    expect(mocks.getShapingStats.mock.lastCall?.[0].dimension).toBe('group')
    expect(wrapper.findAll('tbody tr')).toHaveLength(2)
    wrapper.unmount()
  })
})
