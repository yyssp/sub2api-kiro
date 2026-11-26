import { defineComponent } from 'vue'
import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import UsageMultiplierFields from '@/components/admin/UsageMultiplierFields.vue'
import { createDefaultCacheStrategyConfig } from '../cacheStrategyTemplates'
import { createCacheStrategyDrafts } from '../cacheStrategyEditor'

vi.mock('vue-i18n', () => ({useI18n: () => ({t: (key: string) => key})}))
describe('direct usage multiplier editor', () => {
  for (const protocol of ['anthropic', 'gemini'] as const) {
    it(`${protocol} has neutral defaults and only supported buckets`, () => {
      const config = createDefaultCacheStrategyConfig('usage_multiplier', protocol)
      for (const field of Object.values(config.usage_multiplier!)) expect(field).toEqual({threshold_tokens: 0, multiplier: 1})
      const wrapper = mount(UsageMultiplierFields, {props:{modelValue:config},global:{stubs:{Toggle:true}}})
      expect(wrapper.findAll('input')).toHaveLength(protocol === 'gemini' ? 6 : 8)
      expect(wrapper.find('[data-field="cache_creation-multiplier"]').exists()).toBe(protocol === 'anthropic')
      wrapper.unmount()
    })
    it(`${protocol} allows output thresholds and amplification/reduction`, async () => {
      const config = createDefaultCacheStrategyConfig('usage_multiplier', protocol)
      const Host = defineComponent({components:{UsageMultiplierFields},data:()=>({config}),template:'<UsageMultiplierFields v-model="config" />'})
      const wrapper=mount(Host,{global:{stubs:{Toggle:true}}})
      await wrapper.find('[data-field="output-threshold"]').setValue('1000')
      await wrapper.find('[data-field="output-multiplier"]').setValue('1.5')
      expect(wrapper.vm.config.usage_multiplier!.output).toEqual({threshold_tokens:1000,multiplier:1.5})
      expect(wrapper.text()).toContain('1,800')
      await wrapper.find('[data-field="output-multiplier"]').setValue('0.5')
      expect(wrapper.text()).toContain('600')
      await wrapper.find('[data-field="output-multiplier"]').setValue('0')
      expect(wrapper.vm.config.usage_multiplier!.output.multiplier).toBe(0)
      wrapper.unmount()
    })
  }
  it('preserves direct multiplier rules across protocol and kind round trips', () => {
    const drafts=createCacheStrategyDrafts()
    let c=createDefaultCacheStrategyConfig('usage_multiplier')
    c.usage_multiplier!.output={threshold_tokens:1000,multiplier:1.5}
    c=drafts.switch(c,'anthropic','usage_multiplier','gemini','usage_multiplier')
    c=drafts.switch(c,'gemini','usage_multiplier','anthropic','usage_multiplier')
    expect(c.usage_multiplier!.output).toEqual({threshold_tokens:1000,multiplier:1.5})
    c=drafts.switch(c,'anthropic','usage_multiplier','anthropic','prefix')
    c=drafts.switch(c,'anthropic','prefix','anthropic','usage_multiplier')
    expect(c.usage_multiplier!.output).toEqual({threshold_tokens:1000,multiplier:1.5})
  })
})
