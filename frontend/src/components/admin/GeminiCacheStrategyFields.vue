<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { CacheStrategyConfig } from '@/api/admin/cacheStrategies'
import Select from '@/components/common/Select.vue'
import Toggle from '@/components/common/Toggle.vue'
import CacheShapingFields from './CacheShapingFields.vue'

const config = defineModel<CacheStrategyConfig>({ required: true })
const { t } = useI18n()
const label = (key: string) => t(`admin.cacheStrategies.protocols.${key}`)
const fields = ['input', 'cache_read', 'output'] as const
const modes = computed(() => ['raw', 'preserve', 'sample_max', 'sample_target'].map(value => ({ value, label: label(value) })))
const numeric = ['uncached_input_max_tokens', 'reported_input_min_tokens', 'coverage_ratio', 'read_ratio', 'min_cacheable_tokens', 'uncached_input_min_tokens', 'reported_input_max_tokens', 'default_ttl_seconds', 'state_ttl_seconds', 'max_coverage_tokens', 'max_new_creation_tokens_per_request', 'token_scale', 'scale_min_input_tokens', 'max_simulated_input_tokens', 'max_entries_per_scope', 'max_entries_global', 'estimated_bytes_limit', 'expire_after_idle_seconds'] as const
const flags = ['cache_system', 'cache_tools', 'cache_history', 'cache_tool_results', 'incremental_create_enabled', 'allow_derived_session', 'cold_request_guard', 'preserve_upstream_cache_usage'] as const
const hitFields = ['target_hit_ratio', 'ratio_jitter', 'min_cache_read_tokens', 'max_read_multiplier'] as const
</script>

<template>
  <section class="space-y-5 border-t border-gray-200 pt-5 dark:border-dark-700">
    <p class="rounded-lg bg-primary-50 p-4 text-sm leading-6 text-primary-800 dark:bg-primary-950/20 dark:text-primary-200">{{ label('geminiHint') }}</p>
    <div class="flex items-center justify-between"><h3 class="font-semibold">{{ label('usage') }}</h3><Toggle v-model="config.usage.enabled" /></div>
    <details class="space-y-3"><summary class="cursor-pointer text-sm font-medium">{{ label('usage') }}</summary>
    <div class="grid gap-4 md:grid-cols-3">
      <div v-for="field in fields" :key="field" class="space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-700">
        <h4 class="font-medium">{{ label(field) }}</h4>
        <Select v-model="config.usage[field].mode" :options="modes" />
        <label v-if="config.usage[field].mode === 'sample_max'" class="block text-sm">{{ label('max_tokens') }}<input v-model.number="config.usage[field].max_tokens" type="number" min="0" class="input mt-1" /></label>
        <label v-if="config.usage[field].mode === 'sample_target'" class="block text-sm">{{ label('target_tokens') }}<input v-model.number="config.usage[field].target_tokens" type="number" min="0" class="input mt-1" /></label>
        <label v-if="config.usage[field].mode === 'sample_target'" class="block text-sm">{{ label('normal_max_multiplier') }}<input v-model.number="config.usage[field].normal_max_multiplier" type="number" min="1" max="10" step="0.01" class="input mt-1" /></label>
      </div>
    </div>
    <label class="flex items-center gap-2 text-sm"><input v-model="config.usage.input.move_delta_to_cache_read" type="checkbox" class="checkbox" />{{ label('move_delta') }}</label>
    <label class="flex items-center gap-2 text-sm"><input v-model="config.usage.skip_non_stream_usage_projection" type="checkbox" class="checkbox" />{{ t('admin.cacheStrategies.form.skipNonStreamUsageProjection') }}</label>
    </details>
    <details class="space-y-4"><summary class="cursor-pointer text-sm font-medium">{{ t('admin.cacheStrategies.form.limits') }}</summary>
    <p class="text-xs leading-5 text-gray-500">{{ label('stateHint') }}</p>
    <div class="grid gap-4 md:grid-cols-3">
      <label v-for="key in numeric" :key="key" class="text-sm">{{ label(key) }}<input v-model.number="config[key]" type="number" min="0" :step="key.endsWith('ratio') || key === 'token_scale' ? 0.01 : 1" class="input mt-1" @change="key === 'read_ratio' && (config.ratio_mode = 'independent')" /></label>
      <label class="text-sm">{{ label('scope') }}<Select v-model="config.scope_mode" :options="[{value:'group_session',label:label('groupSession')},{value:'group_account_session',label:label('accountSession')}]" /></label>
    </div>
    <div class="grid gap-3 md:grid-cols-2"><label v-for="key in flags" :key="key" class="flex items-center gap-2 text-sm"><input v-model="config[key]" type="checkbox" class="checkbox" />{{ label(key) }}</label></div>
    </details>
    <div v-if="config.hit_shaping && config.kind !== 'prefix_tool_plus'" class="space-y-3">
      <label class="flex items-center gap-2 text-sm"><input v-model="config.hit_shaping.enabled" type="checkbox" class="checkbox" />{{ label('hit') }}</label>
      <div v-if="config.hit_shaping.enabled" class="grid gap-3 md:grid-cols-4"><label v-for="key in hitFields" :key="key" class="text-sm">{{ t(`admin.cacheStrategies.advanced.${key}`) }}<input v-model.number="config.hit_shaping[key]" type="number" min="0" step="0.01" class="input mt-1" /></label></div>
    </div>
    <div v-if="config.kind === 'stable_segment_cache' && config.stable_segment" class="grid gap-3 md:grid-cols-3">
      <label v-for="key in (['auto_cache_system','auto_cache_tools','auto_cache_history_message_ends'] as const)" :key="key" class="flex items-center gap-2 text-sm"><input v-model="config.stable_segment[key]" type="checkbox" class="checkbox" />{{ t(`admin.cacheStrategies.advanced.${key}`) }}</label>
      <label v-for="key in (['min_total_input_tokens','min_segment_tokens','history_message_limit','reported_input_min_tokens','reported_input_max_tokens'] as const)" :key="key" class="text-sm">{{ t(`admin.cacheStrategies.advanced.${key}`) }}<input v-model.number="config.stable_segment[key]" type="number" min="0" class="input mt-1" /></label>
    </div>
    <CacheShapingFields v-if="config.kind === 'prefix_tool_plus'" v-model="config" />
    <details class="space-y-3"><summary class="cursor-pointer text-sm font-medium">{{ t('admin.cacheStrategies.form.finalCapJitterHint') }}</summary>
      <div class="grid gap-3 md:grid-cols-2"><label v-for="key in (['final_cache_read_jitter_min_tokens','final_cache_read_jitter_max_tokens','final_output_jitter_min_tokens','final_output_jitter_max_tokens','output_uplift_min_tokens'] as const)" :key="key" class="text-sm">{{ label(key) }}<input v-model.number="config.usage[key]" type="number" min="0" class="input mt-1" /></label></div>
    </details>
    <div class="flex flex-wrap gap-4">
      <label class="flex items-center gap-2 text-sm"><input v-model="config.usage.final_output_guard_enabled" type="checkbox" class="checkbox" />{{ label('outputGuard') }}</label>
      <label class="flex items-center gap-2 text-sm"><input v-model="config.usage.output_uplift_enabled" type="checkbox" class="checkbox" />{{ label('outputUpliftEnabled') }}</label>
    </div>
    <div class="grid gap-3 md:grid-cols-3">
      <label class="text-sm">{{ label('finalRead') }}<input v-model.number="config.usage.final_cache_read_max_tokens" type="number" min="0" class="input mt-1" /></label>
      <label class="text-sm">{{ label('finalOutput') }}<input v-model.number="config.usage.final_output_max_tokens" type="number" min="0" class="input mt-1" /></label>
      <label class="text-sm">{{ label('outputUplift') }}<input v-model.number="config.usage.output_uplift_percent" type="number" min="0" max="200" class="input mt-1" /></label>
    </div>
  </section>
</template>
