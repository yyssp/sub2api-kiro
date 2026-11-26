<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { CacheStrategyConfig } from '@/api/admin/cacheStrategies'
const config = defineModel<CacheStrategyConfig>({ required: true })
const { t } = useI18n()
</script>
<template>
  <section v-if="config.limits" class="space-y-3 rounded-lg bg-gray-50 p-4 dark:bg-dark-800">
    <h3 class="text-sm font-semibold">{{ t('admin.cacheStrategies.audit.limits') }}</h3>
    <p class="text-xs leading-5 text-gray-500">{{ t('admin.cacheStrategies.audit.limitsHint') }}</p>
    <div class="grid gap-4 md:grid-cols-2">
      <label class="text-sm">{{ t('admin.cacheStrategies.audit.inputLimit') }}<input v-model.number="config.limits.max_input_tokens" type="number" min="0" max="4000000" class="input mt-1" /></label>
      <label class="text-sm">{{ t('admin.cacheStrategies.audit.outputLimit') }}<input v-model.number="config.limits.max_output_tokens" type="number" min="0" max="4000000" class="input mt-1" /></label>
    </div>
    <p class="text-xs leading-5 text-gray-500">{{ t('admin.cacheStrategies.audit.defaultsHint') }}</p>
  </section>
</template>
