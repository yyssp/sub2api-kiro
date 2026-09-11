<template>
  <AppLayout>
    <TablePageLayout>
      <template #filters>
        <div class="flex flex-col gap-3 lg:flex-row lg:items-center">
          <div class="relative w-full flex-1 sm:max-w-80">
            <Icon
              name="search"
              size="md"
              class="absolute left-3 top-1/2 -translate-y-1/2 text-gray-400 dark:text-dark-400"
            />
            <input
              v-model="searchQuery"
              type="search"
              class="input pl-10"
              :placeholder="t('admin.cacheStrategies.searchPlaceholder')"
              @keyup.enter="load"
            />
          </div>

          <Select
            v-model="filters.kind"
            :options="kindFilterOptions"
            class="w-full sm:w-44"
            @update:model-value="load"
          />
          <Select
            v-model="filters.status"
            :options="statusFilterOptions"
            class="w-full sm:w-40"
            @update:model-value="load"
          />

          <div class="flex flex-1 items-center justify-end gap-2">
            <button
              type="button"
              class="btn btn-secondary"
              :title="t('admin.cacheStrategies.refresh')"
              :disabled="loading"
              @click="load"
            >
              <Icon
                name="refresh"
                size="md"
                :class="{ 'animate-spin': loading }"
              />
              <span class="sr-only">
                {{ t("admin.cacheStrategies.refresh") }}
              </span>
            </button>
            <button type="button" class="btn btn-primary" @click="openCreate">
              <Icon name="plus" size="md" class="mr-1" />
              {{ t("admin.cacheStrategies.create") }}
            </button>
          </div>
        </div>

        <div
          v-if="error"
          class="mt-3 border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700 dark:border-red-900/50 dark:bg-red-950/30 dark:text-red-300"
        >
          {{ error }}
        </div>
      </template>

      <template #table>
        <DataTable
          :columns="columns"
          :data="filteredItems"
          :loading="loading"
          :server-side-sort="false"
          default-sort-key="name"
          default-sort-order="asc"
        >
          <template #cell-name="{ row }">
            <div class="min-w-0">
              <div class="truncate font-medium text-gray-900 dark:text-white">
                {{ row.name }}
              </div>
              <div
                v-if="row.description"
                class="mt-1 max-w-md truncate text-xs text-gray-500 dark:text-dark-400"
              >
                {{ row.description }}
              </div>
            </div>
          </template>

          <template #cell-kind="{ row }">
            <span
              class="badge"
              :class="
                row.config?.kind === 'disabled' ? 'badge-gray' : 'badge-primary'
              "
            >
              {{ kindLabel(row.config?.kind) }}
            </span>
          </template>

          <template #cell-usage_summary="{ row }">
            <div
              class="max-w-xs text-xs leading-5 text-gray-600 dark:text-gray-300"
            >
              <span class="font-medium">{{
                t("admin.cacheStrategies.usageSummaryInput")
              }}</span>
              ·
              {{ usageModeLabel(row.config.usage?.input?.mode) }}
              <span class="mx-1 text-gray-300 dark:text-dark-500">|</span>
              <span class="font-medium">{{
                t("admin.cacheStrategies.usageSummaryOutput")
              }}</span>
              ·
              {{ usageModeLabel(row.config.usage?.output?.mode) }}
              <span class="mx-1 text-gray-300 dark:text-dark-500">|</span>
              <span class="font-medium">{{
                t("admin.cacheStrategies.usageSummaryRead")
              }}</span>
              ·
              {{ usageModeLabel(row.config.usage?.cache_read?.mode) }}
              <span class="mx-1 text-gray-300 dark:text-dark-500">|</span>
              <span class="font-medium">{{
                t("admin.cacheStrategies.usageSummaryCreation")
              }}</span>
              ·
              {{ usageModeLabel(row.config.usage?.cache_creation?.mode) }}
            </div>
          </template>

          <template #cell-enabled="{ value }">
            <span class="badge" :class="value ? 'badge-success' : 'badge-gray'">
              {{
                value
                  ? t("admin.cacheStrategies.enabled")
                  : t("admin.cacheStrategies.disabled")
              }}
            </span>
          </template>

          <template #cell-bound_group_count="{ value }">
            <span
              class="inline-flex items-center rounded bg-gray-100 px-2 py-0.5 text-xs font-medium text-gray-700 dark:bg-dark-600 dark:text-gray-300"
            >
              {{
                value
                  ? t("admin.cacheStrategies.groupCount", { count: value })
                  : t("admin.cacheStrategies.unbound")
              }}
            </span>
          </template>

          <template #cell-revision="{ value }">
            <span class="font-mono text-xs text-gray-500 dark:text-dark-400">
              v{{ value }}
            </span>
          </template>

          <template #cell-actions="{ row }">
            <div class="flex items-center gap-1">
              <button
                type="button"
                class="rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-gray-100 hover:text-gray-700 dark:hover:bg-dark-600 dark:hover:text-gray-200"
                :title="t('common.edit')"
                @click="edit(row)"
              >
                <Icon name="edit" size="sm" />
                <span class="sr-only">
                  {{ t("common.edit") }}
                </span>
              </button>
              <button
                type="button"
                class="rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-gray-100 hover:text-gray-700 dark:hover:bg-dark-600 dark:hover:text-gray-200"
                :title="t('admin.cacheStrategies.duplicate')"
                @click="duplicate(row)"
              >
                <Icon name="copy" size="sm" />
                <span class="sr-only">
                  {{ t("admin.cacheStrategies.duplicate") }}
                </span>
              </button>
              <button
                type="button"
                class="rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-gray-100 hover:text-gray-700 dark:hover:bg-dark-600 dark:hover:text-gray-200"
                :title="
                  row.enabled
                    ? t('admin.cacheStrategies.disable')
                    : t('admin.cacheStrategies.enable')
                "
                @click="toggle(row)"
              >
                <Icon :name="row.enabled ? 'lock' : 'checkCircle'" size="sm" />
                <span class="sr-only">
                  {{
                    row.enabled
                      ? t("admin.cacheStrategies.disable")
                      : t("admin.cacheStrategies.enable")
                  }}
                </span>
              </button>
              <button
                type="button"
                class="rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-900/20 dark:hover:text-red-400"
                :title="t('common.delete')"
                @click="requestRemove(row)"
              >
                <Icon name="trash" size="sm" />
                <span class="sr-only">
                  {{ t("common.delete") }}
                </span>
              </button>
            </div>
          </template>

          <template #empty>
            <EmptyState
              :title="t('admin.cacheStrategies.empty')"
              :description="t('admin.cacheStrategies.description')"
              :action-text="t('admin.cacheStrategies.create')"
              @action="openCreate"
            />
          </template>
        </DataTable>
      </template>
    </TablePageLayout>

    <BaseDialog
      :show="Boolean(editing)"
      :title="
        editing?.id
          ? t('admin.cacheStrategies.edit')
          : t('admin.cacheStrategies.create')
      "
      width="extra-wide"
      @close="closeEditor"
    >
      <form id="cache-strategy-form" class="space-y-6" @submit.prevent="save">
        <section class="space-y-4">
          <div>
            <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
              {{ t("admin.cacheStrategies.form.basic") }}
            </h3>
            <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">
              {{ t("admin.cacheStrategies.form.basicHint") }}
            </p>
          </div>

          <div class="grid gap-4 md:grid-cols-2">
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.name") }}
              </label>
              <input
                v-if="editing"
                v-model="editing.name"
                type="text"
                class="input"
                required
                :placeholder="t('admin.cacheStrategies.name')"
              />
            </div>

            <div v-if="editing && !editing.id">
              <label class="input-label">
                {{ t("admin.cacheStrategies.templateLabel") }}
              </label>
              <Select
                v-model="selectedTemplateId"
                :options="templateOptions"
                @update:model-value="applyTemplate"
              />
              <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">
                {{ t("admin.cacheStrategies.templateHint") }}
              </p>
              <p
                v-if="selectedTemplateId !== 'blank'"
                class="mt-1 text-xs leading-5 text-gray-500 dark:text-dark-400"
              >
                {{ selectedTemplateDescription }}
              </p>
            </div>

            <div v-if="editing">
              <label class="input-label">
                {{ t("admin.cacheStrategies.kind") }}
              </label>
              <Select
                v-model="editing.config.kind"
                :options="kindOptions"
                @update:model-value="handleKindChange"
              />
              <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">
                {{ kindDescription(editing.config.kind) }}
              </p>
            </div>

            <div v-if="editing" class="flex items-center justify-between gap-4">
              <div>
                <label class="input-label mb-0">
                  {{ t("admin.cacheStrategies.status") }}
                </label>
                <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">
                  {{
                    editing.enabled
                      ? t("admin.cacheStrategies.enabled")
                      : t("admin.cacheStrategies.disabled")
                  }}
                </p>
              </div>
              <Toggle v-model="editing.enabled" />
            </div>

            <div class="md:col-span-2">
              <label class="input-label">
                {{ t("admin.cacheStrategies.descriptionLabel") }}
              </label>
              <textarea
                v-if="editing"
                v-model="editing.description"
                rows="2"
                class="input"
                :placeholder="t('admin.cacheStrategies.descriptionPlaceholder')"
              />
            </div>
          </div>
        </section>

        <section
          v-if="editing"
          class="space-y-4 border-t border-gray-200 pt-5 dark:border-dark-700"
        >
          <div class="flex items-start justify-between gap-4">
            <div>
              <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
                {{ t("admin.cacheStrategies.form.usagePolicy") }}
              </h3>
              <p
                class="mt-1 max-w-3xl text-xs leading-5 text-gray-500 dark:text-dark-400"
              >
                {{ t("admin.cacheStrategies.form.usagePolicyHint") }}
              </p>
            </div>
            <Toggle v-model="editing.config.usage.enabled" />
          </div>

          <div
            v-if="editing.config.usage.enabled"
            class="rounded-lg bg-primary-50/60 px-4 py-3 text-xs leading-5 text-primary-800 dark:bg-primary-950/20 dark:text-primary-200"
          >
            {{ t("admin.cacheStrategies.form.usageFlow") }}
            <div class="mt-2">
              {{ t("admin.cacheStrategies.form.usageExample") }}
            </div>
          </div>

          <div v-if="editing.config.usage.enabled">
            <label class="flex items-center gap-2 text-sm">
              <input
                v-model="editing.config.usage.skip_non_stream_usage_projection"
                type="checkbox"
                class="checkbox"
              />
              <span>
                {{
                  t("admin.cacheStrategies.form.skipNonStreamUsageProjection")
                }}
              </span>
            </label>
            <p class="mt-1 text-xs leading-5 text-gray-500 dark:text-dark-400">
              {{
                t("admin.cacheStrategies.form.skipNonStreamUsageProjectionHint")
              }}
            </p>
          </div>

          <div
            v-if="editing.config.usage.enabled"
            class="grid gap-4 xl:grid-cols-2"
          >
            <div
              class="space-y-3 rounded-lg bg-gray-50/60 p-4 dark:bg-dark-800/40"
            >
              <div>
                <h4 class="text-sm font-medium text-gray-900 dark:text-white">
                  {{ t("admin.cacheStrategies.form.usageInput") }}
                </h4>
                <p
                  class="mt-1 text-xs leading-5 text-gray-500 dark:text-dark-400"
                >
                  {{ t("admin.cacheStrategies.form.usageInputHint") }}
                </p>
              </div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.usageMode") }}
              </label>
              <Select
                v-model="editing.config.usage.input.mode"
                :options="usageModeOptions"
              />
              <div
                v-if="editing.config.usage.input.mode === 'sample_max'"
                class="grid gap-3 sm:grid-cols-2"
              >
                <div>
                  <label class="input-label">
                    {{ t("admin.cacheStrategies.form.usageMaxTokens") }}
                  </label>
                  <input
                    v-model.number="editing.config.usage.input.max_tokens"
                    type="number"
                    class="input"
                    min="1"
                  />
                </div>
              </div>
              <div
                v-if="editing.config.usage.input.mode === 'sample_target'"
                class="grid gap-3 sm:grid-cols-2"
              >
                <div>
                  <label class="input-label">
                    {{ t("admin.cacheStrategies.form.usageTargetTokens") }}
                  </label>
                  <input
                    v-model.number="editing.config.usage.input.target_tokens"
                    type="number"
                    class="input"
                    min="1"
                  />
                </div>
                <div>
                  <label class="input-label">
                    {{
                      t("admin.cacheStrategies.form.usageNormalMaxMultiplier")
                    }}
                  </label>
                  <input
                    v-model.number="
                      editing.config.usage.input.normal_max_multiplier
                    "
                    type="number"
                    class="input"
                    min="1"
                    step="0.01"
                  />
                </div>
              </div>
              <label
                class="flex items-center justify-between gap-3 rounded-lg bg-white/80 px-3 py-2.5 dark:bg-dark-900/40"
              >
                <span class="text-sm text-gray-700 dark:text-gray-200">
                  {{ t("admin.cacheStrategies.form.moveInputDelta") }}
                </span>
                <Toggle
                  v-model="editing.config.usage.input.move_delta_to_cache_read"
                />
              </label>
            </div>

            <div
              class="space-y-3 rounded-lg bg-gray-50/60 p-4 dark:bg-dark-800/40"
            >
              <div>
                <h4 class="text-sm font-medium text-gray-900 dark:text-white">
                  {{ t("admin.cacheStrategies.form.usageOutput") }}
                </h4>
                <p
                  class="mt-1 text-xs leading-5 text-gray-500 dark:text-dark-400"
                >
                  {{ t("admin.cacheStrategies.form.usageOutputHint") }}
                </p>
              </div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.usageMode") }}
              </label>
              <Select
                v-model="editing.config.usage.output.mode"
                :options="usageModeOptions"
              />
              <div
                v-if="editing.config.usage.output.mode === 'sample_max'"
                class="grid gap-3 sm:grid-cols-2"
              >
                <div>
                  <label class="input-label">
                    {{ t("admin.cacheStrategies.form.usageMaxTokens") }}
                  </label>
                  <input
                    v-model.number="editing.config.usage.output.max_tokens"
                    type="number"
                    class="input"
                    min="1"
                  />
                </div>
              </div>
              <div
                v-if="editing.config.usage.output.mode === 'sample_target'"
                class="grid gap-3 sm:grid-cols-2"
              >
                <div>
                  <label class="input-label">
                    {{ t("admin.cacheStrategies.form.usageTargetTokens") }}
                  </label>
                  <input
                    v-model.number="editing.config.usage.output.target_tokens"
                    type="number"
                    class="input"
                    min="1"
                  />
                </div>
                <div>
                  <label class="input-label">
                    {{
                      t("admin.cacheStrategies.form.usageNormalMaxMultiplier")
                    }}
                  </label>
                  <input
                    v-model.number="
                      editing.config.usage.output.normal_max_multiplier
                    "
                    type="number"
                    class="input"
                    min="1"
                    step="0.01"
                  />
                </div>
              </div>
              <label class="flex items-center gap-2 text-sm">
                <input
                  v-model="editing.config.usage.output_uplift_enabled"
                  type="checkbox"
                  class="checkbox"
                />
                <span>
                  {{ t("admin.cacheStrategies.form.outputUpliftEnabled") }}
                </span>
              </label>
              <p class="form-hint">
                {{ t("admin.cacheStrategies.form.outputUpliftEnabledHint") }}
              </p>
              <div class="grid gap-3 sm:grid-cols-2">
                <div>
                  <label class="input-label">
                    {{ t("admin.cacheStrategies.form.outputUpliftMinTokens") }}
                  </label>
                  <input
                    v-model.number="
                      editing.config.usage.output_uplift_min_tokens
                    "
                    type="number"
                    class="input"
                    min="0"
                    :disabled="!editing.config.usage.output_uplift_enabled"
                  />
                </div>
                <div>
                  <label class="input-label">
                    {{ t("admin.cacheStrategies.form.outputUpliftPercent") }}
                  </label>
                  <input
                    v-model.number="editing.config.usage.output_uplift_percent"
                    type="number"
                    class="input"
                    min="0"
                    max="200"
                    :disabled="!editing.config.usage.output_uplift_enabled"
                  />
                </div>
              </div>
              <label class="flex items-center gap-2 text-sm">
                <input
                  v-model="editing.config.usage.final_output_guard_enabled"
                  type="checkbox"
                  class="checkbox"
                />
                <span>
                  {{ t("admin.cacheStrategies.form.finalOutputGuardEnabled") }}
                </span>
              </label>
              <p class="text-xs leading-5 text-gray-500 dark:text-dark-400">
                {{
                  t("admin.cacheStrategies.form.finalOutputGuardEnabledHint")
                }}
              </p>
              <div>
                <label class="input-label">
                  {{ t("admin.cacheStrategies.form.finalOutputMaxTokens") }}
                </label>
                <input
                  v-model.number="editing.config.usage.final_output_max_tokens"
                  type="number"
                  class="input"
                  min="0"
                />
              </div>
              <div class="grid gap-3 sm:grid-cols-2">
                <div>
                  <label class="input-label">
                    {{
                      t("admin.cacheStrategies.form.finalOutputJitterMinTokens")
                    }}
                  </label>
                  <input
                    v-model.number="
                      editing.config.usage.final_output_jitter_min_tokens
                    "
                    type="number"
                    class="input"
                    min="0"
                  />
                </div>
                <div>
                  <label class="input-label">
                    {{
                      t("admin.cacheStrategies.form.finalOutputJitterMaxTokens")
                    }}
                  </label>
                  <input
                    v-model.number="
                      editing.config.usage.final_output_jitter_max_tokens
                    "
                    type="number"
                    class="input"
                    min="0"
                  />
                </div>
              </div>
              <p class="text-xs leading-5 text-gray-500 dark:text-dark-400">
                {{ t("admin.cacheStrategies.form.finalCapJitterHint") }}
              </p>
            </div>

            <div
              class="space-y-3 rounded-lg bg-gray-50/60 p-4 dark:bg-dark-800/40"
            >
              <div>
                <h4 class="text-sm font-medium text-gray-900 dark:text-white">
                  {{ t("admin.cacheStrategies.form.usageCacheRead") }}
                </h4>
                <p
                  class="mt-1 text-xs leading-5 text-gray-500 dark:text-dark-400"
                >
                  {{ t("admin.cacheStrategies.form.usageCacheReadHint") }}
                </p>
              </div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.usageMode") }}
              </label>
              <Select
                v-model="editing.config.usage.cache_read.mode"
                :options="usageModeOptions"
              />
              <div
                v-if="editing.config.usage.cache_read.mode === 'sample_max'"
                class="grid gap-3 sm:grid-cols-2"
              >
                <div>
                  <label class="input-label">
                    {{ t("admin.cacheStrategies.form.usageMaxTokens") }}
                  </label>
                  <input
                    v-model.number="editing.config.usage.cache_read.max_tokens"
                    type="number"
                    class="input"
                    min="1"
                  />
                </div>
              </div>
              <div
                v-if="editing.config.usage.cache_read.mode === 'sample_target'"
                class="grid gap-3 sm:grid-cols-2"
              >
                <div>
                  <label class="input-label">
                    {{ t("admin.cacheStrategies.form.usageTargetTokens") }}
                  </label>
                  <input
                    v-model.number="
                      editing.config.usage.cache_read.target_tokens
                    "
                    type="number"
                    class="input"
                    min="1"
                  />
                </div>
                <div>
                  <label class="input-label">
                    {{
                      t("admin.cacheStrategies.form.usageNormalMaxMultiplier")
                    }}
                  </label>
                  <input
                    v-model.number="
                      editing.config.usage.cache_read.normal_max_multiplier
                    "
                    type="number"
                    class="input"
                    min="1"
                    step="0.01"
                  />
                </div>
              </div>
              <div>
                <label class="input-label">
                  {{ t("admin.cacheStrategies.form.finalCacheReadMaxTokens") }}
                </label>
                <input
                  v-model.number="
                    editing.config.usage.final_cache_read_max_tokens
                  "
                  type="number"
                  class="input"
                  min="0"
                />
              </div>
              <div class="grid gap-3 sm:grid-cols-2">
                <div>
                  <label class="input-label">
                    {{
                      t(
                        "admin.cacheStrategies.form.finalCacheReadJitterMinTokens",
                      )
                    }}
                  </label>
                  <input
                    v-model.number="
                      editing.config.usage.final_cache_read_jitter_min_tokens
                    "
                    type="number"
                    class="input"
                    min="0"
                  />
                </div>
                <div>
                  <label class="input-label">
                    {{
                      t(
                        "admin.cacheStrategies.form.finalCacheReadJitterMaxTokens",
                      )
                    }}
                  </label>
                  <input
                    v-model.number="
                      editing.config.usage.final_cache_read_jitter_max_tokens
                    "
                    type="number"
                    class="input"
                    min="0"
                  />
                </div>
              </div>
              <p class="text-xs leading-5 text-gray-500 dark:text-dark-400">
                {{ t("admin.cacheStrategies.form.finalCapJitterHint") }}
              </p>
            </div>

            <div
              class="space-y-3 rounded-lg bg-gray-50/60 p-4 dark:bg-dark-800/40"
            >
              <div>
                <h4 class="text-sm font-medium text-gray-900 dark:text-white">
                  {{ t("admin.cacheStrategies.form.usageCacheCreation") }}
                </h4>
                <p
                  class="mt-1 text-xs leading-5 text-gray-500 dark:text-dark-400"
                >
                  {{ t("admin.cacheStrategies.form.usageCacheCreationHint") }}
                </p>
              </div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.usageMode") }}
              </label>
              <Select
                v-model="editing.config.usage.cache_creation.mode"
                :options="usageModeOptions"
              />
              <div
                v-if="editing.config.usage.cache_creation.mode === 'sample_max'"
                class="grid gap-3 sm:grid-cols-2"
              >
                <div>
                  <label class="input-label">
                    {{ t("admin.cacheStrategies.form.usageMaxTokens") }}
                  </label>
                  <input
                    v-model.number="
                      editing.config.usage.cache_creation.max_tokens
                    "
                    type="number"
                    class="input"
                    min="1"
                  />
                </div>
              </div>
              <div
                v-if="
                  editing.config.usage.cache_creation.mode === 'sample_target'
                "
                class="grid gap-3 sm:grid-cols-2"
              >
                <div>
                  <label class="input-label">
                    {{ t("admin.cacheStrategies.form.usageTargetTokens") }}
                  </label>
                  <input
                    v-model.number="
                      editing.config.usage.cache_creation.target_tokens
                    "
                    type="number"
                    class="input"
                    min="1"
                  />
                </div>
                <div>
                  <label class="input-label">
                    {{
                      t("admin.cacheStrategies.form.usageNormalMaxMultiplier")
                    }}
                  </label>
                  <input
                    v-model.number="
                      editing.config.usage.cache_creation.normal_max_multiplier
                    "
                    type="number"
                    class="input"
                    min="1"
                    step="0.01"
                  />
                </div>
              </div>
              <div>
                <label class="input-label">
                  {{
                    t("admin.cacheStrategies.form.finalCacheCreationMaxTokens")
                  }}
                </label>
                <input
                  v-model.number="
                    editing.config.usage.final_cache_creation_max_tokens
                  "
                  type="number"
                  class="input"
                  min="0"
                />
              </div>
              <div class="grid gap-3 sm:grid-cols-2">
                <div>
                  <label class="input-label">
                    {{
                      t(
                        "admin.cacheStrategies.form.finalCacheCreationJitterMinTokens",
                      )
                    }}
                  </label>
                  <input
                    v-model.number="
                      editing.config.usage
                        .final_cache_creation_jitter_min_tokens
                    "
                    type="number"
                    class="input"
                    min="0"
                  />
                </div>
                <div>
                  <label class="input-label">
                    {{
                      t(
                        "admin.cacheStrategies.form.finalCacheCreationJitterMaxTokens",
                      )
                    }}
                  </label>
                  <input
                    v-model.number="
                      editing.config.usage
                        .final_cache_creation_jitter_max_tokens
                    "
                    type="number"
                    class="input"
                    min="0"
                  />
                </div>
              </div>
              <p class="text-xs leading-5 text-gray-500 dark:text-dark-400">
                {{ t("admin.cacheStrategies.form.finalCapJitterHint") }}
              </p>
            </div>
          </div>
        </section>

        <section
          v-if="editing"
          class="space-y-4 border-t border-gray-200 pt-5 dark:border-dark-700"
        >
          <div>
            <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
              {{ t("admin.cacheStrategies.form.behavior") }}
            </h3>
            <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">
              {{ t("admin.cacheStrategies.form.behaviorHint") }}
            </p>
          </div>

          <div class="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.coverageRatio") }}
              </label>
              <input
                v-model.number="editing.config.coverage_ratio"
                type="number"
                class="input"
                min="0"
                max="1"
                step="0.01"
              />
            </div>
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.usageRatio") }}
              </label>
              <input
                v-model.number="editing.config.usage_ratio"
                type="number"
                class="input"
                min="0"
                max="1"
                step="0.01"
              />
            </div>
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.ratioMode") }}
              </label>
              <Select
                v-model="editing.config.ratio_mode"
                :options="ratioModeOptions"
              />
            </div>
            <div v-if="editing.config.ratio_mode === 'independent'">
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.readRatio") }}
              </label>
              <input
                v-model.number="editing.config.read_ratio"
                type="number"
                class="input"
                min="0"
                max="1"
                step="0.01"
              />
            </div>
            <div v-if="editing.config.ratio_mode === 'independent'">
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.creationRatio") }}
              </label>
              <input
                v-model.number="editing.config.creation_ratio"
                type="number"
                class="input"
                min="0"
                max="1"
                step="0.01"
              />
            </div>
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.breakpointMode") }}
              </label>
              <Select
                v-model="editing.config.breakpoint_mode"
                :options="breakpointOptions"
              />
            </div>
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.scopeMode") }}
              </label>
              <Select
                v-model="editing.config.scope_mode"
                :options="scopeModeOptions"
              />
            </div>
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.dynamicContentMode") }}
              </label>
              <Select
                v-model="editing.config.dynamic_content_mode"
                :options="dynamicContentModeOptions"
              />
            </div>
            <label
              class="flex items-center justify-between gap-3 rounded-lg bg-gray-50/60 px-3 py-2.5 dark:bg-dark-800/40"
            >
              <span class="text-sm text-gray-700 dark:text-gray-200">
                {{ t("admin.cacheStrategies.form.allowDerivedSession") }}
              </span>
              <Toggle v-model="editing.config.allow_derived_session" />
            </label>
          </div>
        </section>

        <section
          v-if="editing"
          class="space-y-4 border-t border-gray-200 pt-5 dark:border-dark-700"
        >
          <div>
            <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
              {{ t("admin.cacheStrategies.form.limits") }}
            </h3>
            <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">
              {{ t("admin.cacheStrategies.form.limitsHint") }}
            </p>
          </div>

          <div class="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.minCacheableTokens") }}
              </label>
              <input
                v-model.number="editing.config.min_cacheable_tokens"
                type="number"
                class="input"
                min="0"
              />
            </div>
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.maxCoverageTokens") }}
              </label>
              <input
                v-model.number="editing.config.max_coverage_tokens"
                type="number"
                class="input"
                min="0"
              />
            </div>
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.maxCreationTokens") }}
              </label>
              <input
                v-model.number="
                  editing.config.max_new_creation_tokens_per_request
                "
                type="number"
                class="input"
                min="0"
              />
            </div>
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.defaultTtlSeconds") }}
              </label>
              <input
                v-model.number="editing.config.default_ttl_seconds"
                type="number"
                class="input"
                min="1"
                max="3600"
              />
            </div>
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.hourTtlSeconds") }}
              </label>
              <input
                v-model.number="editing.config.hour_ttl_seconds"
                type="number"
                class="input"
                min="1"
                max="3600"
              />
            </div>
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.tokenScale") }}
              </label>
              <input
                v-model.number="editing.config.token_scale"
                type="number"
                class="input"
                min="1"
                max="3"
                step="0.01"
              />
            </div>
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.scaleMinInputTokens") }}
              </label>
              <input
                v-model.number="editing.config.scale_min_input_tokens"
                type="number"
                class="input"
                min="0"
              />
            </div>
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.maxSimulatedInputTokens") }}
              </label>
              <input
                v-model.number="editing.config.max_simulated_input_tokens"
                type="number"
                class="input"
                min="0"
              />
            </div>
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.capJitterMinTokens") }}
              </label>
              <input
                v-model.number="editing.config.cap_jitter_min_tokens"
                type="number"
                class="input"
                min="0"
              />
            </div>
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.capJitterMaxTokens") }}
              </label>
              <input
                v-model.number="editing.config.cap_jitter_max_tokens"
                type="number"
                class="input"
                min="0"
              />
            </div>
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.maxEntriesPerScope") }}
              </label>
              <input
                v-model.number="editing.config.max_entries_per_scope"
                type="number"
                class="input"
                min="0"
              />
            </div>
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.maxEntriesGlobal") }}
              </label>
              <input
                v-model.number="editing.config.max_entries_global"
                type="number"
                class="input"
                min="0"
              />
            </div>
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.estimatedBytesLimit") }}
              </label>
              <input
                v-model.number="editing.config.estimated_bytes_limit"
                type="number"
                class="input"
                min="0"
              />
            </div>
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.expireAfterIdleSeconds") }}
              </label>
              <input
                v-model.number="editing.config.expire_after_idle_seconds"
                type="number"
                class="input"
                min="0"
              />
            </div>
          </div>
        </section>

        <section
          v-if="editing"
          class="space-y-4 border-t border-gray-200 pt-5 dark:border-dark-700"
        >
          <div>
            <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
              {{ t("admin.cacheStrategies.form.segments") }}
            </h3>
            <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">
              {{ t("admin.cacheStrategies.form.segmentsHint") }}
            </p>
          </div>

          <div class="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
            <label
              class="flex items-center justify-between gap-3 rounded-lg bg-gray-50/60 px-3 py-2.5 dark:bg-dark-800/40"
            >
              <span class="text-sm text-gray-700 dark:text-gray-200">
                {{ t("admin.cacheStrategies.form.cacheSystem") }}
              </span>
              <Toggle v-model="editing.config.cache_system" />
            </label>
            <label
              class="flex items-center justify-between gap-3 rounded-lg bg-gray-50/60 px-3 py-2.5 dark:bg-dark-800/40"
            >
              <span class="text-sm text-gray-700 dark:text-gray-200">
                {{ t("admin.cacheStrategies.form.cacheTools") }}
              </span>
              <Toggle v-model="editing.config.cache_tools" />
            </label>
            <label
              class="flex items-center justify-between gap-3 rounded-lg bg-gray-50/60 px-3 py-2.5 dark:bg-dark-800/40"
            >
              <span class="text-sm text-gray-700 dark:text-gray-200">
                {{ t("admin.cacheStrategies.form.cacheHistory") }}
              </span>
              <Toggle v-model="editing.config.cache_history" />
            </label>
            <label
              class="flex items-center justify-between gap-3 rounded-lg bg-gray-50/60 px-3 py-2.5 dark:bg-dark-800/40"
            >
              <span class="text-sm text-gray-700 dark:text-gray-200">
                {{ t("admin.cacheStrategies.form.cacheToolResults") }}
              </span>
              <Toggle v-model="editing.config.cache_tool_results" />
            </label>
            <label
              class="flex items-center justify-between gap-3 rounded-lg bg-gray-50/60 px-3 py-2.5 dark:bg-dark-800/40"
            >
              <span class="text-sm text-gray-700 dark:text-gray-200">
                {{ t("admin.cacheStrategies.form.currentUserStablePrefix") }}
              </span>
              <Toggle
                v-model="editing.config.cache_current_user_stable_prefix"
              />
            </label>
            <label
              class="flex items-center justify-between gap-3 rounded-lg bg-gray-50/60 px-3 py-2.5 dark:bg-dark-800/40"
            >
              <span class="text-sm text-gray-700 dark:text-gray-200">
                {{ t("admin.cacheStrategies.form.incrementalCreation") }}
              </span>
              <Toggle v-model="editing.config.incremental_create_enabled" />
            </label>
          </div>

          <div
            v-if="editing.config.cache_current_user_stable_prefix"
            class="max-w-sm"
          >
            <label class="input-label">
              {{
                t("admin.cacheStrategies.form.currentUserStablePrefixMaxTokens")
              }}
            </label>
            <input
              v-model.number="
                editing.config.current_user_stable_prefix_max_tokens
              "
              type="number"
              class="input"
              min="0"
            />
          </div>
        </section>

        <section
          v-if="editing"
          class="space-y-4 border-t border-gray-200 pt-5 dark:border-dark-700"
        >
          <div>
            <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
              {{ t("admin.cacheStrategies.form.creationControl") }}
            </h3>
            <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">
              {{ t("admin.cacheStrategies.form.creationControlHint") }}
            </p>
          </div>

          <div
            class="flex items-center justify-between gap-4 rounded-lg bg-gray-50/60 px-3 py-2.5 dark:bg-dark-800/40"
          >
            <span class="text-sm text-gray-700 dark:text-gray-200">
              {{ t("admin.cacheStrategies.form.creationControlEnabled") }}
            </span>
            <Toggle v-model="editing.config.creation_control.enabled" />
          </div>

          <div
            v-if="editing.config.creation_control.enabled"
            class="grid gap-4 md:grid-cols-2 lg:grid-cols-3"
          >
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.minCreationDeltaTokens") }}
              </label>
              <input
                v-model.number="
                  editing.config.creation_control.min_creation_delta_tokens
                "
                type="number"
                class="input"
                min="0"
              />
            </div>
            <div>
              <label class="input-label">
                {{
                  t("admin.cacheStrategies.form.minSuccessfulRequestsBetween")
                }}
              </label>
              <input
                v-model.number="
                  editing.config.creation_control
                    .min_successful_requests_between
                "
                type="number"
                class="input"
                min="0"
              />
            </div>
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.minCreationIntervalSeconds") }}
              </label>
              <input
                v-model.number="
                  editing.config.creation_control.min_creation_interval_seconds
                "
                type="number"
                class="input"
                min="0"
              />
            </div>
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.maxCreationTokensPerEvent") }}
              </label>
              <input
                v-model.number="
                  editing.config.creation_control.max_creation_tokens_per_event
                "
                type="number"
                class="input"
                min="0"
              />
            </div>
            <div>
              <label class="input-label">
                {{
                  t("admin.cacheStrategies.form.creationBudgetWindowSeconds")
                }}
              </label>
              <input
                v-model.number="
                  editing.config.creation_control.creation_budget_window_seconds
                "
                type="number"
                class="input"
                min="0"
              />
            </div>
            <div>
              <label class="input-label">
                {{ t("admin.cacheStrategies.form.maxCreationTokensPerWindow") }}
              </label>
              <input
                v-model.number="
                  editing.config.creation_control.max_creation_tokens_per_window
                "
                type="number"
                class="input"
                min="0"
              />
            </div>
          </div>
        </section>

        <section
          v-if="editing"
          class="space-y-4 border-t border-gray-200 pt-5 dark:border-dark-700"
        >
          <div>
            <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
              {{ t("admin.cacheStrategies.form.bindings") }}
            </h3>
            <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">
              {{ t("admin.cacheStrategies.form.bindingsHint") }}
            </p>
            <p class="mt-1 text-xs leading-5 text-gray-500 dark:text-dark-400">
              {{ t("admin.cacheStrategies.form.cacheNamespaceHint") }}
            </p>
          </div>

          <GroupSelector
            v-model="selectedGroupIds"
            :groups="groups"
            :label="t('admin.cacheStrategies.boundGroups')"
            searchable="auto"
          />
        </section>
      </form>

      <template #footer>
        <div class="flex justify-end gap-2">
          <button type="button" class="btn btn-secondary" @click="closeEditor">
            {{ t("common.cancel") }}
          </button>
          <button
            type="submit"
            form="cache-strategy-form"
            class="btn btn-primary"
            :disabled="saving"
          >
            {{ saving ? t("common.saving") : t("common.save") }}
          </button>
        </div>
      </template>
    </BaseDialog>

    <ConfirmDialog
      :show="showDeleteConfirm"
      :title="t('admin.cacheStrategies.delete')"
      :message="
        t('admin.cacheStrategies.deleteConfirm', {
          name: deleting?.name,
        })
      "
      :confirm-text="t('common.delete')"
      :cancel-text="t('common.cancel')"
      :danger="true"
      @confirm="confirmRemove"
      @cancel="showDeleteConfirm = false"
    />
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { useI18n } from "vue-i18n";

import BaseDialog from "@/components/common/BaseDialog.vue";
import ConfirmDialog from "@/components/common/ConfirmDialog.vue";
import DataTable from "@/components/common/DataTable.vue";
import EmptyState from "@/components/common/EmptyState.vue";
import GroupSelector from "@/components/common/GroupSelector.vue";
import Select from "@/components/common/Select.vue";
import Toggle from "@/components/common/Toggle.vue";
import type { Column } from "@/components/common/types";
import Icon from "@/components/icons/Icon.vue";
import AppLayout from "@/components/layout/AppLayout.vue";
import TablePageLayout from "@/components/layout/TablePageLayout.vue";
import api, {
  type CacheStrategy,
  type CacheStrategyConfig,
} from "@/api/admin/cacheStrategies";
import groupsAPI from "@/api/admin/groups";
import { useAppStore } from "@/stores/app";
import type { AdminGroup } from "@/types";
import { extractI18nErrorMessage } from "@/utils/apiError";
import {
  cacheStrategyTemplates,
  createDefaultCacheStrategyConfig,
  type CacheStrategyTemplateId,
} from "./cacheStrategyTemplates";

const { t } = useI18n();
const appStore = useAppStore();

const items = ref<Array<CacheStrategy & { bound_group_count: number }>>([]);
const editing = ref<CacheStrategy | null>(null);
const deleting = ref<CacheStrategy | null>(null);
const groups = ref<AdminGroup[]>([]);
const selectedGroupIds = ref<number[]>([]);
const searchQuery = ref("");
const loading = ref(false);
const saving = ref(false);
const error = ref("");
const showDeleteConfirm = ref(false);
const selectedTemplateId = ref<CacheStrategyTemplateId>("blank");
const lastSelectedKind = ref<CacheStrategyConfig["kind"]>("prefix");

const filters = reactive({
  kind: "",
  status: "",
});

const kindOptions = computed(() => [
  {
    value: "prefix",
    label: t("admin.cacheStrategies.kinds.prefix"),
  },
  {
    value: "tool_aware",
    label: t("admin.cacheStrategies.kinds.toolAware"),
  },
  {
    value: "disabled",
    label: t("admin.cacheStrategies.kinds.disabled"),
  },
]);

const kindFilterOptions = computed(() => [
  {
    value: "",
    label: t("admin.cacheStrategies.allKinds"),
  },
  ...kindOptions.value,
]);

const statusFilterOptions = computed(() => [
  {
    value: "",
    label: t("admin.cacheStrategies.allStatuses"),
  },
  {
    value: "enabled",
    label: t("admin.cacheStrategies.enabled"),
  },
  {
    value: "disabled",
    label: t("admin.cacheStrategies.disabled"),
  },
]);

const ratioModeOptions = computed(() => [
  {
    value: "uniform",
    label: t("admin.cacheStrategies.ratioModes.uniform"),
  },
  {
    value: "independent",
    label: t("admin.cacheStrategies.ratioModes.independent"),
  },
]);

const breakpointOptions = computed(() => [
  {
    value: "client_only",
    label: t("admin.cacheStrategies.breakpointModes.clientOnly"),
  },
  {
    value: "hybrid",
    label: t("admin.cacheStrategies.breakpointModes.hybrid"),
  },
  {
    value: "auto",
    label: t("admin.cacheStrategies.breakpointModes.auto"),
  },
]);

// 默认项排在最前：分组 + 会话。
const scopeModeOptions = computed(() => [
  {
    value: "group_session",
    label: t("admin.cacheStrategies.scopeModes.groupSession"),
  },
  {
    value: "group_account_session",
    label: t("admin.cacheStrategies.scopeModes.groupAccountSession"),
  },
]);

const dynamicContentModeOptions = computed(() => [
  {
    value: "exclude",
    label: t("admin.cacheStrategies.dynamicContentModes.exclude"),
  },
  {
    value: "allow",
    label: t("admin.cacheStrategies.dynamicContentModes.allow"),
  },
]);

const usageModeOptions = computed(() => [
  {
    value: "raw",
    label: t("admin.cacheStrategies.usageModes.raw"),
  },
  {
    value: "preserve",
    label: t("admin.cacheStrategies.usageModes.preserve"),
  },
  {
    value: "sample_max",
    label: t("admin.cacheStrategies.usageModes.sampleMax"),
  },
  {
    value: "sample_target",
    label: t("admin.cacheStrategies.usageModes.sampleTarget"),
  },
]);

const columns = computed<Column[]>(() => [
  {
    key: "name",
    label: t("admin.cacheStrategies.name"),
    sortable: true,
  },
  {
    key: "kind",
    label: t("admin.cacheStrategies.kind"),
    sortable: true,
  },
  {
    key: "usage_summary",
    label: t("admin.cacheStrategies.usageSummary"),
  },
  {
    key: "enabled",
    label: t("admin.cacheStrategies.status"),
    sortable: true,
  },
  {
    key: "bound_group_count",
    label: t("admin.cacheStrategies.boundGroups"),
    sortable: true,
  },
  {
    key: "revision",
    label: t("admin.cacheStrategies.revision"),
    sortable: true,
  },
  {
    key: "actions",
    label: t("admin.cacheStrategies.actions"),
  },
]);

const filteredItems = computed(() => {
  return items.value.filter((item) => {
    const matchesKind = !filters.kind || item.config.kind === filters.kind;
    const matchesStatus =
      !filters.status ||
      (filters.status === "enabled" && item.enabled) ||
      (filters.status === "disabled" && !item.enabled);
    return matchesKind && matchesStatus;
  });
});

function defaults(kind: CacheStrategyConfig["kind"] = "prefix") {
  return createDefaultCacheStrategyConfig(kind);
}

const templateOptions = computed(() => [
  {
    value: "blank",
    label: t("admin.cacheStrategies.templates.blank.name"),
  },
  ...cacheStrategyTemplates.map((template) => ({
    value: template.id,
    label: t(template.nameKey),
  })),
]);

const selectedTemplateDescription = computed(() => {
  const template = cacheStrategyTemplates.find(
    (candidate) => candidate.id === selectedTemplateId.value,
  );
  return template ? t(template.descriptionKey) : "";
});

function kindDescription(kind: CacheStrategyConfig["kind"]) {
  if (kind === "tool_aware") {
    return t("admin.cacheStrategies.kindDescriptions.toolAware");
  }
  if (kind === "disabled") {
    return t("admin.cacheStrategies.kindDescriptions.disabled");
  }
  return t("admin.cacheStrategies.kindDescriptions.prefix");
}

function kindLabel(kind: CacheStrategyConfig["kind"]) {
  if (kind === "tool_aware") {
    return t("admin.cacheStrategies.kinds.toolAware");
  }
  if (kind === "disabled") {
    return t("admin.cacheStrategies.kinds.disabled");
  }
  return t("admin.cacheStrategies.kinds.prefix");
}

function usageModeLabel(mode?: string) {
  switch (mode) {
    case "preserve":
      return t("admin.cacheStrategies.usageModes.preserveShort");
    case "sample_max":
      return t("admin.cacheStrategies.usageModes.sampleMaxShort");
    case "sample_target":
      return t("admin.cacheStrategies.usageModes.sampleTargetShort");
    case "raw":
    default:
      return t("admin.cacheStrategies.usageModes.rawShort");
  }
}

function openCreate() {
  selectedGroupIds.value = [];
  selectedTemplateId.value = "blank";
  lastSelectedKind.value = "prefix";
  editing.value = {
    id: 0,
    name: "",
    description: "",
    enabled: true,
    revision: 0,
    config: defaults(),
    created_at: "",
    updated_at: "",
  };
}

async function load() {
  loading.value = true;
  error.value = "";

  try {
    items.value = await api.list(searchQuery.value.trim());
  } catch (e: any) {
    error.value = extractI18nErrorMessage(
      e,
      t,
      "admin.cacheStrategies",
      t("admin.cacheStrategies.loadFailed"),
    );
  } finally {
    loading.value = false;
  }
}

async function edit(item: CacheStrategy) {
  selectedTemplateId.value = "blank";
  const cloned = JSON.parse(JSON.stringify(item)) as CacheStrategy;
  editing.value = cloned;
  lastSelectedKind.value = cloned.config.kind;

  try {
    selectedGroupIds.value = (await api.groups(item.id)).map(
      (group) => group.id,
    );
  } catch (e: any) {
    selectedGroupIds.value = [];
    appStore.showError(e?.message || t("admin.cacheStrategies.loadFailed"));
  }
}

function closeEditor() {
  editing.value = null;
  selectedGroupIds.value = [];
  selectedTemplateId.value = "blank";
  lastSelectedKind.value = "prefix";
}

function applyTemplate(value: string | number | boolean | null) {
  if (!editing.value || editing.value.id) {
    return;
  }

  if (typeof value !== "string" && typeof value !== "number") {
    return;
  }
  const templateId = String(value) as CacheStrategyTemplateId;
  selectedTemplateId.value = templateId;
  if (templateId === "blank") {
    editing.value.config = defaults();
    editing.value.name = "";
    editing.value.description = "";
    lastSelectedKind.value = "prefix";
    return;
  }

  const template = cacheStrategyTemplates.find(
    (candidate) => candidate.id === templateId,
  );
  if (!template) {
    return;
  }

  editing.value.config = template.createConfig();
  editing.value.name = t(template.nameKey);
  editing.value.description = t(template.descriptionKey);
  lastSelectedKind.value = editing.value.config.kind;
}

function handleKindChange(value: string | number | boolean | null) {
  if (!editing.value) {
    return;
  }

  if (typeof value !== "string" && typeof value !== "number") {
    return;
  }
  const nextKind = String(value) as CacheStrategyConfig["kind"];
  const previousKind = lastSelectedKind.value;
  if (nextKind === "disabled" || previousKind === "disabled") {
    editing.value.config = defaults(nextKind);
  } else {
    editing.value.config.kind = nextKind;
  }
  lastSelectedKind.value = nextKind;
}

async function save() {
  if (!editing.value) {
    return;
  }

  saving.value = true;
  error.value = "";

  try {
    const item = editing.value;
    const payload = {
      name: item.name,
      description: item.description,
      enabled: item.enabled,
      config: item.config,
    };

    let saved: CacheStrategy;
    if (item.id) {
      saved = await api.update(item.id, {
        ...payload,
        expected_revision: item.revision,
      });
    } else {
      saved = await api.create(payload);
    }

    if (saved.id) {
      if (item.id) {
        await api.replaceGroups(saved.id, selectedGroupIds.value);
      } else {
        await api.bindGroups(saved.id, selectedGroupIds.value);
      }
    }

    closeEditor();
    appStore.showSuccess(t("admin.cacheStrategies.saveSuccess"));
    await load();
  } catch (e: any) {
    const message = extractI18nErrorMessage(
      e,
      t,
      "admin.cacheStrategies",
      t("admin.cacheStrategies.saveFailed"),
    );
    error.value = message;
    appStore.showError(message);
  } finally {
    saving.value = false;
  }
}

async function duplicate(item: CacheStrategy) {
  try {
    await api.duplicate(
      item.id,
      t("admin.cacheStrategies.copyName", { name: item.name }),
    );
    appStore.showSuccess(t("admin.cacheStrategies.copySuccess"));
    await load();
  } catch (e: any) {
    const message = extractI18nErrorMessage(
      e,
      t,
      "admin.cacheStrategies",
      t("admin.cacheStrategies.duplicateFailed"),
    );
    error.value = message;
    appStore.showError(message);
  }
}

async function toggle(item: CacheStrategy) {
  try {
    await api.setEnabled(item.id, !item.enabled);
    appStore.showSuccess(t("admin.cacheStrategies.toggleSuccess"));
    await load();
  } catch (e: any) {
    const message = extractI18nErrorMessage(
      e,
      t,
      "admin.cacheStrategies",
      t("admin.cacheStrategies.toggleFailed"),
    );
    error.value = message;
    appStore.showError(message);
  }
}

function requestRemove(item: CacheStrategy) {
  deleting.value = item;
  showDeleteConfirm.value = true;
}

async function confirmRemove() {
  if (!deleting.value) {
    return;
  }

  try {
    await api.remove(deleting.value.id);
    showDeleteConfirm.value = false;
    deleting.value = null;
    appStore.showSuccess(t("admin.cacheStrategies.deleteSuccess"));
    await load();
  } catch (e: any) {
    const message = extractI18nErrorMessage(
      e,
      t,
      "admin.cacheStrategies",
      t("admin.cacheStrategies.deleteFailed"),
    );
    error.value = message;
    appStore.showError(message);
  }
}

onMounted(async () => {
  await Promise.all([
    load(),
    groupsAPI
      .getAllIncludingInactive()
      .then((value) => {
        groups.value = value;
      })
      .catch(() => {
        groups.value = [];
      }),
  ]);
});
</script>
