<script setup lang="ts">
import { computed } from "vue";
import { useI18n } from "vue-i18n";
import type { CacheStrategyConfig, CacheShapingProfile } from "@/api/admin/cacheStrategies";
import Select, { type SelectOption } from "@/components/common/Select.vue";
const config = defineModel<CacheStrategyConfig>({ required: true });
const { t } = useI18n();
const label = (key: string) => t(`admin.cacheStrategies.advanced.${key}`);
const hitNumbers = ["target_hit_ratio", "ratio_jitter", "min_cache_read_tokens", "min_cache_creation_tokens", "min_cost_ratio", "max_read_multiplier", "min_creation_ratio"] as const;
const hitFlags = ["enabled", "read_aware_cost_floor", "cost_floor_top_up_input"] as const;
const stableNumbers = ["min_total_input_tokens", "min_segment_tokens", "reported_input_min_tokens", "reported_input_max_tokens", "history_message_limit"] as const;
const stableFlags = ["honor_explicit_cache_control", "auto_cache_system", "auto_cache_tools", "auto_cache_history_message_ends"] as const;
const gemini = computed(() => config.value.protocol === "gemini");
const fields = computed(() => gemini.value ? (["input", "output", "cache_read"] as const) : (["input", "output", "cache_read", "cache_creation"] as const));
const shapingModeOptions = computed<SelectOption[]>(() => ["inherit", "custom", "raw"].map(value => ({ value, label: label(value) })));
const suppressedTargetOptions = computed<SelectOption[]>(() => ["input", "cache_read", "drop"].map(value => ({ value, label: label(value) })));
const shapingKeys = ["multiplier", "jitter_ratio", "min_tokens", "max_tokens", "max_jitter_tokens"] as const;
const profiles = computed<Array<{ key: string; value: CacheShapingProfile }>>(() => {
  const p = config.value.tool_plus;
  if (!p) return [];
  const result = [{ key: "defaultProfile", value: p.shaping }];
  if (p.non_stream.mode === "custom") result.push({ key: "nonStreamProfile", value: p.non_stream.profile });
  if (p.small_request.enabled && p.small_request.mode === "custom") result.push({ key: "smallProfile", value: p.small_request.profile });
  return result;
});
</script>

<template>
  <section class="space-y-4 rounded-lg border border-gray-200 p-4 dark:border-dark-600">
    <h4 class="font-medium">{{ label("title") }}</h4>
    <div v-if="!gemini" class="grid gap-3 md:grid-cols-2">
      <label class="flex items-center gap-2 text-sm"><input v-model="config.usage.strict_cache_eligibility_stream" type="checkbox" class="checkbox" />{{ label("strictStream") }}</label>
      <label class="flex items-center gap-2 text-sm"><input v-model="config.usage.strict_cache_eligibility_non_stream" type="checkbox" class="checkbox" />{{ label("strictNonStream") }}</label>
      <label class="flex items-center gap-2 text-sm"><input v-model="config.cold_request_guard" type="checkbox" class="checkbox" />{{ label("coldGuard") }}</label>
      <label class="text-sm">{{ label("stateTTL") }}<input v-model.number="config.state_ttl_seconds" type="number" min="0" max="300" class="input mt-1 w-full" /></label>
    </div>
    <p v-if="!gemini" class="text-xs text-gray-500">{{ label("eligibilityHint") }}</p>
    <template v-if="!gemini && config.kind !== 'prefix_tool_plus' && config.hit_shaping">
      <h5 class="font-medium">{{ label("hitTitle") }}</h5>
      <div class="flex flex-wrap gap-4">
        <label v-for="key in hitFlags" :key="key" class="flex items-center gap-2 text-sm"><input v-model="config.hit_shaping[key]" type="checkbox" class="checkbox" />{{ label(key) }}</label>
      </div>
      <div v-if="config.hit_shaping.enabled" class="grid gap-3 md:grid-cols-3">
        <label v-for="key in hitNumbers" :key="key" class="text-sm">{{ label(key) }}<input v-model.number="config.hit_shaping[key]" type="number" min="0" :step="key.includes('tokens') ? 1 : 0.01" class="input mt-1 w-full" /></label>
      </div>
      <p v-if="config.kind === 'bounded_high_cache'" class="text-xs text-gray-500">{{ label("boundedHint") }}</p>
    </template>
    <template v-if="!gemini && config.kind === 'stable_segment_cache' && config.stable_segment">
      <h5 class="font-medium">{{ label("stableTitle") }}</h5>
      <div class="grid gap-3 md:grid-cols-3"><label v-for="key in stableNumbers" :key="key" class="text-sm">{{ label(key) }}<input v-model.number="config.stable_segment[key]" type="number" min="0" step="1" class="input mt-1 w-full" /></label></div>
      <div class="grid gap-3 md:grid-cols-2"><label v-for="key in stableFlags" :key="key" class="flex items-center gap-2 text-sm"><input v-model="config.stable_segment[key]" type="checkbox" class="checkbox" />{{ label(key) }}</label></div>
    </template>
    <template v-if="config.kind === 'prefix_tool_plus' && config.tool_plus">
      <h5 class="font-medium">{{ label("plusTitle") }}</h5>
      <div class="grid gap-3 md:grid-cols-2">
        <div class="text-sm"><label for="cache-plus-non-stream-mode" class="input-label">{{ label("nonStreamMode") }}</label><Select id="cache-plus-non-stream-mode" v-model="config.tool_plus.non_stream.mode" :options="shapingModeOptions" class="mt-1 w-full" :aria-label="label('nonStreamMode')" /></div>
        <div v-if="!gemini" class="text-sm"><label for="cache-plus-suppressed-target" class="input-label">{{ label("suppressedTarget") }}</label><Select id="cache-plus-suppressed-target" v-model="config.tool_plus.suppressed_creation_target" :options="suppressedTargetOptions" class="mt-1 w-full" :aria-label="label('suppressedTarget')" /></div>
        <label class="flex items-center gap-2 text-sm"><input v-model="config.tool_plus.small_request.enabled" type="checkbox" class="checkbox" />{{ label("smallEnabled") }}</label>
      </div>
      <div v-if="config.tool_plus.small_request.enabled" class="grid gap-3 md:grid-cols-3">
        <label class="text-sm">{{ label("smallMax") }}<input v-model.number="config.tool_plus.small_request.max_input_tokens" type="number" min="0" class="input mt-1 w-full" /></label>
        <div class="text-sm"><label for="cache-plus-small-mode" class="input-label">{{ label("smallMode") }}</label><Select id="cache-plus-small-mode" v-model="config.tool_plus.small_request.mode" :options="shapingModeOptions" class="mt-1 w-full" :aria-label="label('smallMode')" /></div>
        <label class="flex items-center gap-2 text-sm"><input v-model="config.tool_plus.small_request.disable_cache" type="checkbox" class="checkbox" />{{ label("disableCache") }}</label>
      </div>
      <div v-for="profile in profiles" :key="profile.key" class="space-y-3 rounded bg-gray-50 p-3 dark:bg-dark-800">
        <h5 class="text-sm font-medium">{{ label(profile.key) }}</h5>
        <div class="overflow-x-auto"><table class="w-full text-sm"><thead><tr><th></th><th v-for="key in shapingKeys" :key="key" class="p-1 text-left">{{ label(key) }}</th></tr></thead><tbody><tr v-for="field in fields" :key="field"><td class="pr-2">{{ label(field) }}</td><td v-for="key in shapingKeys" :key="key" class="p-1"><input v-model.number="profile.value[field][key]" type="number" min="0" :step="key === 'multiplier' || key === 'jitter_ratio' ? 0.01 : 1" class="input w-24" /></td></tr></tbody></table></div>
        <div class="grid gap-3 md:grid-cols-2"><label class="flex items-center gap-2 text-sm"><input v-model="profile.value.cap_overflow_to_input" type="checkbox" class="checkbox" />{{ label("overflowToInput") }}</label><label v-if="!gemini" class="text-sm">{{ label("min_cost_ratio") }}<input v-model.number="profile.value.min_cost_ratio" type="number" min="0" max="100" step="0.01" class="input mt-1 w-full" /></label></div>
      </div>
    </template>
  </section>
</template>
