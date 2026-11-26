import { describe, expect, it } from 'vitest'
import { createCacheStrategyDrafts, completeCacheStrategyConfig } from '../cacheStrategyEditor'
import { createDefaultCacheStrategyConfig } from '../cacheStrategyTemplates'

describe('cache strategy draft transitions', () => {
  it('restores each protocol and mode including disabled mode without dropping limits', () => {
    const drafts = createCacheStrategyDrafts()
    let config = createDefaultCacheStrategyConfig()
    config.forced_ttl_tier = '1h'
    config.creation_control.min_creation_interval_seconds = 9
    config.limits = {max_input_tokens: 12345, max_output_tokens: 678}
    config.protocol = 'gemini' // Vue v-model updates before the handler.
    config = drafts.switch(config, 'anthropic', 'prefix', 'gemini', 'prefix')
    expect(config.creation_control.enabled).toBe(false)
    expect(config.creation_ratio).toBe(0)
    config.usage.output.max_tokens = 321
    config.protocol = 'anthropic'
    config = drafts.switch(config, 'gemini', 'prefix', 'anthropic', 'prefix')
    expect(config.forced_ttl_tier).toBe('1h')
    expect(config.creation_control.min_creation_interval_seconds).toBe(9)
    config.kind = 'disabled'
    config = drafts.switch(config, 'anthropic', 'prefix', 'anthropic', 'disabled')
    expect(config.usage.enabled).toBe(false)
    expect(config.limits?.max_input_tokens).toBe(12345)
    config.kind = 'prefix'
    config = drafts.switch(config, 'anthropic', 'disabled', 'anthropic', 'prefix')
    expect(config.usage.enabled).toBe(true)
    expect(config.creation_control.min_creation_interval_seconds).toBe(9)
    config.protocol = 'gemini'
    config = drafts.switch(config, 'anthropic', 'prefix', 'gemini', 'prefix')
    expect(config.usage.output.max_tokens).toBe(321)
    expect(config.limits).toEqual({max_input_tokens: 12345, max_output_tokens: 678})
  })
  it('fills nested legacy values while preserving explicit zero and false', () => {
    const config = {protocol:'gemini',kind:'prefix_tool_plus',allow_derived_session:false,limits:{max_input_tokens:12},tool_plus:{shaping:{input:{multiplier:0}}}}
    const completed = completeCacheStrategyConfig(config as ReturnType<typeof createDefaultCacheStrategyConfig>)
    expect(completed.allow_derived_session).toBe(false)
    expect(completed.limits).toEqual({max_input_tokens:12,max_output_tokens:0})
    expect(completed.tool_plus?.shaping.input.multiplier).toBe(0)
    expect(completed.tool_plus?.non_stream.mode).toBe('inherit')
    expect(completed.creation_ratio).toBe(0)
  })
})
