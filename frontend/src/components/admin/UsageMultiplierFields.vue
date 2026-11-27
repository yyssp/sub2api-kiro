<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { CacheStrategyConfig } from '@/api/admin/cacheStrategies'
import Toggle from '@/components/common/Toggle.vue'
const config = defineModel<CacheStrategyConfig>({ required: true })
const { t } = useI18n()
const fields = computed(() => config.value.protocol === 'gemini'
  ? ['input', 'output', 'cache_read'] as const : ['input', 'output', 'cache_read', 'cache_creation'] as const)
const label = (key: string) => t(`admin.cacheStrategies.multiplier.${key}`)
const example = (field: string, threshold: number, multiplier: number) => Math.max(field === 'input' || field === 'output' ? 1 : 0, 1200 > threshold ? Math.round(1200 * multiplier) : 1200)
</script>

<template>
  <section v-if="config.usage_multiplier" class="space-y-4 border-t border-gray-200 pt-5 dark:border-dark-700">
    <div class="flex items-center justify-between gap-4"><h3 class="font-semibold">{{ label('title') }}</h3><Toggle v-model="config.usage.enabled" :aria-label="label('enabled')" /></div>
    <p class="text-sm leading-6 text-gray-600 dark:text-gray-300">{{ label('hint') }}</p>
    <p class="text-sm">{{ label('help') }}</p>
    <div class="grid gap-4 md:grid-cols-2">
      <div v-for="field in fields" :key="field" class="space-y-3 rounded-lg border border-gray-200 p-4 dark:border-dark-700">
        <h4 class="font-medium">{{ label(field) }}</h4>
        <label class="block text-sm">{{ label('threshold') }}<input v-model.number="config.usage_multiplier[field].threshold_tokens" :data-field="`${field}-threshold`" type="number" required min="0" max="4000000" step="1" class="input mt-1" /></label>
        <label class="block text-sm">{{ label('factor') }}<input v-model.number="config.usage_multiplier[field].multiplier" :data-field="`${field}-multiplier`" type="number" required min="0" max="1000" step="0.01" class="input mt-1" /></label>
        <p class="text-xs text-gray-500">{{ label('preview') }}：1,200 → {{ example(field, config.usage_multiplier[field].threshold_tokens, config.usage_multiplier[field].multiplier).toLocaleString() }}</p>
      </div>
    </div>
    <p class="text-xs leading-5 text-gray-500">{{ label('example') }}</p>
    <p v-if="config.protocol === 'gemini'" class="rounded-lg bg-primary-50 p-3 text-sm dark:bg-primary-950/20">{{ label('gemini') }}</p>
  </section>
</template>
