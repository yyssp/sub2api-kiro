import type { CacheStrategyConfig, CacheStrategyProtocol } from '@/api/admin/cacheStrategies'
import { createDefaultCacheStrategyConfig } from './cacheStrategyTemplates'

const clone = <T>(value: T): T => JSON.parse(JSON.stringify(value)) as T

export function createCacheStrategyDrafts() {
  const drafts = new Map<string, CacheStrategyConfig>()
  return {
    clear: () => drafts.clear(),
    switch(current: CacheStrategyConfig, previousProtocol: CacheStrategyProtocol, previousKind: CacheStrategyConfig['kind'], protocol: CacheStrategyProtocol, kind: CacheStrategyConfig['kind']) {
      const previous = clone(current)
      previous.protocol = previousProtocol
      previous.kind = previousKind
      drafts.set(`${previousProtocol}/${previousKind}`, previous)
      const next = clone(drafts.get(`${protocol}/${kind}`) ?? createDefaultCacheStrategyConfig(kind, protocol))
      // A mode change must not silently remove an administrator's hard usage limits.
      next.limits = clone(current.limits ?? { max_input_tokens: 0, max_output_tokens: 0 })
      return next
    },
  }
}

export function completeCacheStrategyConfig(config: CacheStrategyConfig): CacheStrategyConfig {
  const base = createDefaultCacheStrategyConfig(config.kind, config.protocol ?? 'anthropic')
  function merge(defaults: Record<string, unknown>, value: Record<string, unknown>): Record<string, unknown> {
    const result = { ...defaults }
    for (const [key, entry] of Object.entries(value)) {
      if (entry == null) continue
      const fallback = defaults[key]
      result[key] = typeof entry === 'object' && !Array.isArray(entry) && fallback && typeof fallback === 'object'
        ? merge(fallback as Record<string, unknown>, entry as Record<string, unknown>) : entry
    }
    return result
  }
  return merge(base as unknown as Record<string, unknown>, clone(config) as unknown as Record<string, unknown>) as unknown as CacheStrategyConfig
}
