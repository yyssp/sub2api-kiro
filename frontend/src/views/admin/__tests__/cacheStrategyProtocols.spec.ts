import { describe, expect, it } from 'vitest'
import { createDefaultCacheStrategyConfig, cacheStrategyTemplates } from '../cacheStrategyTemplates'
import { cacheStrategyProtocol, configureCacheProtocol, cacheStrategyProtocols, filterCacheStrategyPlatforms } from '../cacheStrategyProtocols'

describe('cache strategy protocol modules', () => {
  it('keeps legacy policies on Anthropic', () => {
    expect(cacheStrategyProtocol({})).toBe('anthropic')
    expect(createDefaultCacheStrategyConfig().protocol).toBe('anthropic')
  })
  it('uses the existing platform identifiers and declares write support', () => {
    expect(cacheStrategyProtocols.map(p => [p.id, p.supportsCacheCreation])).toEqual([['anthropic', true], ['gemini', false]])
  })
  it('filters the shared platform catalog to platforms with cache shaping support', () => {
    const platforms = [
      { value: 'anthropic', label: 'Anthropic' },
      { value: 'openai', label: 'OpenAI' },
      { value: 'gemini', label: 'Gemini' },
      { value: 'cursor', label: 'Cursor' },
    ]
    expect(filterCacheStrategyPlatforms(platforms)).toEqual([platforms[0], platforms[2]])
  })
  it('preserves Gemini selection across every template and serialization', () => {
    for (const template of cacheStrategyTemplates) {
      const config = configureCacheProtocol(template.createConfig(), 'gemini')
      expect(JSON.parse(JSON.stringify(config)).protocol).toBe('gemini')
      expect(config.allow_derived_session).toBe(true)
      expect(config.creation_control.enabled).toBe(false)
      expect(config.forced_ttl_tier).toBe('')
    }
  })
})


describe('protocol switching preserves authored settings', () => {
  it('keeps session, breakpoint and eligibility preferences', () => {
    const config = createDefaultCacheStrategyConfig()
    config.allow_derived_session = false
    config.breakpoint_mode = 'client_only'
    config.usage.strict_cache_eligibility_stream = true
    configureCacheProtocol(config, 'gemini')
    expect(config.allow_derived_session).toBe(false)
    expect(config.breakpoint_mode).toBe('client_only')
    expect(config.usage.strict_cache_eligibility_stream).toBe(true)
  })
})
