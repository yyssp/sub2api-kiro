import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import CacheStrategiesView from '../CacheStrategiesView.vue'
import { createDefaultCacheStrategyConfig } from '../cacheStrategyTemplates'

const { api, showError } = vi.hoisted(() => ({api: {list: vi.fn(), groups: vi.fn(), create: vi.fn(), update: vi.fn(), replaceGroups: vi.fn(), bindGroups: vi.fn()}, showError: vi.fn()}))
vi.mock('@/api/admin/cacheStrategies', () => ({default: api}))
vi.mock('@/api/admin/groups', () => ({default: {getAllIncludingInactive: vi.fn().mockResolvedValue([])}}))
vi.mock('@/stores/app', () => ({useAppStore: () => ({showSuccess: vi.fn(),showError})}))
vi.mock('vue-i18n', async () => ({...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),useI18n: () => ({t: (key: string) => key})}))
const passthrough = defineComponent({template: '<div><slot/><slot name="filters"/><slot name="table"/><slot name="footer"/></div>'})
const dialog = defineComponent({props: ['show'],template:'<div v-if="show" role="dialog"><slot/><slot name="footer"/></div>'})
const table = defineComponent({props: ['data'],template:'<div><slot name="cell-actions" v-for="row in data" :key="row.id" :row="row"/></div>'})
const select = defineComponent({props:['modelValue','options'],emits:['update:modelValue'],template:'<select :value="modelValue" @change="$emit(\'update:modelValue\', $event.target.value)"><option v-for="o in options" :key="o.value" :value="o.value">{{ o.label }}</option></select>'})
function row(id=1,revision=1) {return {id,revision,name:'strategy',description:'',enabled:true,config:createDefaultCacheStrategyConfig(),created_at:'',updated_at:'',bound_group_count:1}}
function mountView() {
 return mount(CacheStrategiesView,{global:{stubs:{AppLayout:passthrough,TablePageLayout:passthrough,BaseDialog:dialog,DataTable:table,Select:select,ConfirmDialog:true,GroupSelector:true,Toggle:true,Icon:true,EmptyState:true,CacheShapingFields:true,GeminiCacheStrategyFields:true,CacheUsageLimitsFields:true}}})
}
async function clickText(wrapper: ReturnType<typeof mountView>, text: string) {
 const button=wrapper.findAll('button').find(b=>b.text()===text)
 expect(button).toBeTruthy();await button!.trigger('click');await flushPromises()
}
beforeEach(()=>{vi.clearAllMocks();api.list.mockResolvedValue([]);api.groups.mockResolvedValue([]);api.create.mockResolvedValue(row(20));api.update.mockResolvedValue(row(20,2));api.replaceGroups.mockResolvedValue(undefined)})
describe('cache strategy editor recovery',()=>{
 it('retries binding after create without creating a second strategy',async()=>{
  const w=mountView();await flushPromises();await clickText(w,'admin.cacheStrategies.create')
  await w.find('input[required]').setValue('test');api.replaceGroups.mockRejectedValueOnce(new Error('conflict'))
  await w.find('form').trigger('submit');await flushPromises();expect(api.create).toHaveBeenCalledTimes(1);expect(w.find('[role=dialog]').exists()).toBe(true);expect(w.find('[role=alert]').text()).toContain('admin.cacheStrategies.audit.partialSave')
  await w.find('form').trigger('submit');await flushPromises();expect(api.create).toHaveBeenCalledTimes(1);expect(api.update).toHaveBeenCalledWith(20,expect.objectContaining({expected_revision:1}));expect(api.replaceGroups).toHaveBeenCalledTimes(2)
  w.unmount()
 })
 it('uses the new revision when update succeeds but bindings fail',async()=>{
  api.list.mockResolvedValue([row(20)]);api.groups.mockResolvedValue([{id:8}]);const w=mountView();await flushPromises();await clickText(w,'common.edit')
  api.replaceGroups.mockRejectedValueOnce(new Error('conflict'));await w.find('form').trigger('submit');await flushPromises();await w.find('form').trigger('submit');await flushPromises()
  expect(api.update.mock.calls[1][1].expected_revision).toBe(2);expect(api.replaceGroups.mock.calls[1]).toEqual([20,[8]]);w.unmount()
 })
 it('blocks saving when existing bindings fail to load',async()=>{
  api.list.mockResolvedValue([row()]);api.groups.mockRejectedValueOnce(new Error('network'));const w=mountView();await flushPromises();await clickText(w,'common.edit')
  expect(w.find('fieldset').attributes('disabled')).toBeDefined();await w.find('form').trigger('submit');await flushPromises();expect(api.update).not.toHaveBeenCalled();expect(api.replaceGroups).not.toHaveBeenCalled();w.unmount()
 })
 it('ignores a late binding response after closing and creating another draft',async()=>{
  let resolve: (v: Array<{id:number}>)=>void = ()=>{};api.list.mockResolvedValue([row()]);api.groups.mockImplementationOnce(()=>new Promise(r=>{resolve=r}))
  const w=mountView();await flushPromises();await clickText(w,'common.edit');await clickText(w,'common.cancel');await clickText(w,'admin.cacheStrategies.create')
  resolve([{id:99}]);await flushPromises();await w.find('input[required]').setValue('new');await w.find('form').trigger('submit');await flushPromises();expect(api.replaceGroups).toHaveBeenCalledWith(20,[]);w.unmount()
 })
 it('does not duplicate saves or close a draft while saving',async()=>{
  let resolve: (v: ReturnType<typeof row>)=>void=()=>{};api.create.mockImplementationOnce(()=>new Promise(r=>{resolve=r}));const w=mountView();await flushPromises();await clickText(w,'admin.cacheStrategies.create')
  await w.find('input[required]').setValue('new');await w.find('form').trigger('submit');await w.find('form').trigger('submit');expect(api.create).toHaveBeenCalledTimes(1);expect(w.find('fieldset').attributes('disabled')).toBeDefined()
  resolve(row(20));await flushPromises();w.unmount()
 })
})

it('creates a direct multiplier strategy using the compact editor', async () => {
 const w=mountView();await flushPromises();await clickText(w,'admin.cacheStrategies.create')
 const selector=w.find('[role=dialog]').findAll('select').find(s=>s.findAll('option').some(o=>o.attributes('value')==='usage_multiplier'))!
 await selector.setValue('usage_multiplier')
 expect(w.find('[data-field="output-multiplier"]').exists()).toBe(true)
 expect(w.find('cache-shaping-fields-stub').exists()).toBe(false)
 await w.find('input[required]').setValue('direct')
 await w.find('[data-field="output-threshold"]').setValue('1000');await w.find('[data-field="output-multiplier"]').setValue('1.5')
 await w.find('form').trigger('submit');await flushPromises()
 expect(api.create).toHaveBeenCalledWith(expect.objectContaining({config:expect.objectContaining({kind:'usage_multiplier',usage_multiplier:expect.objectContaining({output:{threshold_tokens:1000,multiplier:1.5}})})}))
 w.unmount()
})


it('ignores hidden jitter settings when saving a multiplier strategy', async () => {
 const item=row(20)
 item.config=createDefaultCacheStrategyConfig('usage_multiplier')
 item.config.usage.final_cache_read_max_tokens=100
 item.config.usage.final_cache_read_jitter_min_tokens=200
 item.config.usage.final_cache_read_jitter_max_tokens=300
 api.list.mockResolvedValue([item])
 const w=mountView();await flushPromises();await clickText(w,'common.edit')
 await w.find('form').trigger('submit');await flushPromises()
 expect(api.update).toHaveBeenCalled()
 w.unmount()
})
