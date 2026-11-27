import type { CacheStrategyConfig, CacheStrategyProtocol } from '@/api/admin/cacheStrategies'

export type { CacheStrategyProtocol } from '@/api/admin/cacheStrategies'

export const cacheStrategyProtocols = [
  { id: 'anthropic', label: 'Anthropic · Claude Code', supportsCacheCreation: true },
  { id: 'gemini', label: 'Gemini · GenerateContent', supportsCacheCreation: false },
] as const satisfies readonly { id: CacheStrategyProtocol; label: string; supportsCacheCreation: boolean }[]

export function filterCacheStrategyPlatforms<T extends { value: string }>(platforms: readonly T[]): T[] {
  const supported = new Set<string>(cacheStrategyProtocols.map(({ id }) => id))
  return platforms.filter(({ value }) => supported.has(value))
}

export function cacheStrategyProtocol(config: Pick<CacheStrategyConfig, 'protocol'>): CacheStrategyProtocol {
  return config.protocol ?? 'anthropic'
}

export function configureCacheProtocol(config: CacheStrategyConfig, protocol: CacheStrategyProtocol): CacheStrategyConfig {
  config.protocol = protocol
  if (protocol === 'gemini') {
    config.forced_ttl_tier = ''
    config.creation_control.enabled = false
  }
  return config
}

export const geminiCacheKinds = ['usage_multiplier', 'prefix', 'tool_aware', 'stable_segment_cache', 'prefix_tool_plus', 'bounded_high_cache'] as const
