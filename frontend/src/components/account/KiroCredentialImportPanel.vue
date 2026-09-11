<template>
  <div class="space-y-4">
    <!-- 按数据形态分 tab：用户拿到的是一坨数据，他知道那是 JSON 还是一串 ksk，
         但未必分得清那是「KAM 导出」还是「kiro.rs 凭证」，所以不按来源分。 -->
    <div>
      <div class="flex gap-1 rounded-lg bg-gray-100 p-1 dark:bg-dark-800" role="tablist">
        <button
          v-for="opt in sourceOptions"
          :key="opt.value"
          type="button"
          role="tab"
          :aria-selected="source === opt.value"
          class="flex-1 rounded-md px-3 py-1.5 text-sm font-medium transition-colors"
          :class="source === opt.value
            ? 'bg-white text-primary-700 shadow-sm dark:bg-dark-700 dark:text-primary-300'
            : 'text-gray-600 hover:text-gray-900 dark:text-dark-300 dark:hover:text-white'"
          @click="selectSource(opt.value)"
        >
          {{ opt.label }}
        </button>
      </div>
      <p class="mt-2 text-xs text-gray-500 dark:text-dark-400">{{ currentSourceHint }}</p>
    </div>

    <!-- 文件选择 + 拖拽 -->
    <div>
      <label class="input-label">{{ t('admin.accounts.kiroImportFile') }}</label>
      <div
        class="flex items-center justify-between gap-3 rounded-lg border border-dashed px-4 py-3 transition-colors"
        :class="dragActive
          ? 'border-primary-400 bg-primary-50/70 dark:border-primary-500 dark:bg-primary-900/20'
          : 'border-gray-300 bg-gray-50 dark:border-dark-600 dark:bg-dark-800'"
        @dragenter.prevent="dragActive = true"
        @dragover.prevent
        @dragleave.prevent="dragActive = false"
        @drop.prevent="handleDrop"
      >
        <div class="min-w-0">
          <div class="truncate text-sm text-gray-700 dark:text-dark-200">
            {{ fileName || t('admin.accounts.kiroImportSelectFile') }}
          </div>
          <div class="text-xs text-gray-500 dark:text-dark-400">{{ acceptHint }}</div>
        </div>
        <button type="button" class="btn btn-secondary shrink-0" @click="openFilePicker">
          {{ t('common.chooseFile') }}
        </button>
      </div>
      <input
        ref="fileInput"
        type="file"
        class="hidden"
        :accept="acceptAttr"
        @change="handleFileChange"
      />
    </div>

    <!-- 内容粘贴 -->
    <div>
      <label class="input-label">{{ t('admin.accounts.kiroImportContent') }}</label>
      <textarea
        v-model="content"
        rows="8"
        class="input font-mono text-xs"
        :placeholder="currentPlaceholder"
        @input="resetPreview()"
      ></textarea>
    </div>

    <!-- 只有 Kiro IDE 导出会缺 clientId/clientSecret，缺了就刷不了 token。 -->
    <div v-if="needsDeviceRegistration">
      <label class="input-label">
        {{ t('admin.accounts.oauth.kiro.deviceRegistrationLabel') }}
        <span class="text-red-500">*</span>
      </label>
      <textarea
        v-model="deviceRegistrationJson"
        rows="4"
        class="input font-mono text-xs"
        placeholder='{"clientId":"...","clientSecret":"..."}'
      ></textarea>
      <p class="input-hint">{{ t('admin.accounts.oauth.kiro.deviceRegistrationHint') }}</p>
    </div>

    <div
      v-if="parseError"
      class="rounded-lg border border-red-200 bg-red-50 p-3 text-sm text-red-600 dark:border-red-800 dark:bg-red-900/20 dark:text-red-400"
    >
      {{ parseError }}
    </div>

    <!-- 解析预览：导入前先确认补齐结果，避免脏数据直接落库。 -->
    <div
      v-if="entries.length"
      class="space-y-3 rounded-xl border border-gray-200 p-4 dark:border-dark-700"
    >
      <div class="flex items-center justify-between">
        <div class="text-sm font-medium text-gray-900 dark:text-white">
          {{ t('admin.accounts.kiroImportPreview', { count: entries.length }) }}
        </div>
        <label class="flex items-center gap-2 text-xs text-gray-600 dark:text-dark-300">
          <input v-model="skipDisabled" type="checkbox" class="rounded" />
          {{ t('admin.accounts.kiroImportSkipDisabled') }}
        </label>
      </div>

      <div class="max-h-64 overflow-auto">
        <table class="w-full text-left text-xs">
          <thead class="text-gray-500 dark:text-dark-400">
            <tr>
              <th class="px-2 py-1 font-medium">{{ t('admin.accounts.kiroImportColName') }}</th>
              <th class="px-2 py-1 font-medium">{{ t('admin.accounts.kiroImportColAuthMethod') }}</th>
              <th class="px-2 py-1 font-medium">{{ t('admin.accounts.kiroImportColRegion') }}</th>
              <th class="px-2 py-1 font-medium">{{ t('admin.accounts.kiroImportColEndpoint') }}</th>
              <th class="px-2 py-1 font-medium">{{ t('admin.accounts.kiroImportColStatus') }}</th>
            </tr>
          </thead>
          <tbody class="text-gray-700 dark:text-dark-200">
            <tr
              v-for="(entry, idx) in entries"
              :key="idx"
              class="border-t border-gray-100 dark:border-dark-700"
              :class="{ 'opacity-50': skipDisabled && entry.disabled }"
            >
              <td class="px-2 py-1">
                <input
                  v-model="entryNames[idx]"
                  class="w-40 rounded border border-gray-200 bg-white px-1.5 py-0.5 text-xs dark:border-dark-600 dark:bg-dark-800"
                />
              </td>
              <td class="px-2 py-1">
                <span class="rounded bg-gray-100 px-1.5 py-0.5 dark:bg-dark-700">{{ entry.auth_method }}</span>
              </td>
              <td class="px-2 py-1">{{ entry.region || '-' }}</td>
              <td class="px-2 py-1">{{ entry.endpoint || '-' }}</td>
              <td class="px-2 py-1">
                <span v-if="entry.disabled" class="text-amber-600 dark:text-amber-400">
                  {{ t('admin.accounts.kiroImportDisabled') }}
                </span>
                <span v-else class="text-emerald-600 dark:text-emerald-400">
                  {{ t('admin.accounts.kiroImportEnabled') }}
                </span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <p class="text-xs text-gray-500 dark:text-dark-400">
        {{ t('admin.accounts.kiroImportParamsHint') }}
      </p>
    </div>

    <!-- 跳过的条目：坏数据不该中断整批导入，但也不能静默消失。 -->
    <div
      v-if="skippedEntries.length"
      class="space-y-2 rounded-xl border border-amber-200 bg-amber-50 p-4 dark:border-amber-800 dark:bg-amber-900/20"
    >
      <div class="text-sm font-medium text-amber-800 dark:text-amber-300">
        {{ t('admin.accounts.kiroImportSkipped', { count: skippedEntries.length }) }}
      </div>
      <div class="max-h-40 space-y-1 overflow-auto">
        <div
          v-for="item in skippedEntries"
          :key="item.index"
          class="text-xs text-amber-700 dark:text-amber-400"
        >
          <span class="font-medium">#{{ item.index }}</span>
          {{ item.reason }}
          <code v-if="item.sample" class="ml-1 opacity-70">{{ item.sample }}</code>
        </div>
      </div>
    </div>

    <div
      v-if="error"
      class="rounded-lg border border-red-200 bg-red-50 p-3 dark:border-red-700 dark:bg-red-900/30"
    >
      <p class="whitespace-pre-line text-sm text-red-600 dark:text-red-400">{{ error }}</p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { KiroCredentialImport, KiroImportSource } from '@/composables/useKiroCredentialImport'

/**
 * Kiro 凭证导入的纯展示层。
 *
 * 状态全部来自 useKiroCredentialImport，本组件不发起创建账号：
 * 账号参数（分组/代理/优先级…）由宿主表单提供，见 CreateAccountModal。
 */
const props = defineProps<{
  importer: KiroCredentialImport
  /** 宿主表单的错误信息（例如批量创建失败），与解析错误分开展示。 */
  error?: string
}>()

// importer 本身是一组 ref，解构出来直接用：模板里改的是这些 ref，
// 不是在改 prop —— 宿主与本组件共享同一份解析状态，这是刻意的。
const {
  source,
  content,
  deviceRegistrationJson,
  fileName,
  parseError,
  entries,
  entryNames,
  skippedEntries,
  skipDisabled,
  acceptAttr,
  acceptHint,
  needsDeviceRegistration,
  resetPreview,
  selectSource,
  loadFile
} = props.importer

const { t } = useI18n()

const fileInput = ref<HTMLInputElement | null>(null)
const dragActive = ref(false)

const sourceOptions = computed(() => [
  { value: 'json' as KiroImportSource, label: t('admin.accounts.kiroImportTabJson') },
  { value: 'apikey' as KiroImportSource, label: t('admin.accounts.kiroImportTabApiKey') },
  { value: 'token' as KiroImportSource, label: t('admin.accounts.kiroImportTabToken') },
  { value: 'kiro_ide' as KiroImportSource, label: t('admin.accounts.kiroImportTabKiroIde') }
])

const currentSourceHint = computed(() => {
  switch (source.value) {
    case 'json':
      return t('admin.accounts.kiroImportHintJson')
    case 'apikey':
      return t('admin.accounts.kiroImportHintApiKey')
    case 'token':
      return t('admin.accounts.kiroImportHintToken')
    default:
      return t('admin.accounts.kiroImportHintKiroIde')
  }
})

const currentPlaceholder = computed(() => {
  switch (source.value) {
    case 'json':
      return '[\n  { "refreshToken": "...", "authMethod": "social" },\n  { "kiroApiKey": "ksk_..." }\n]'
    case 'apikey':
      return 'ksk_xxxxxxxx\nksk_yyyyyyyy|us-east-2\n# 井号开头为注释'
    case 'token':
      return 'eyJhbGciOi...\neyJhbGciOi...'
    default:
      return '{\n  "accessToken": "...",\n  "refreshToken": "...",\n  "profileArn": "..."\n}'
  }
})

const openFilePicker = () => fileInput.value?.click()

const handleFileChange = async (e: Event) => {
  const file = (e.target as HTMLInputElement).files?.[0]
  if (file) await loadFile(file)
  // 清空以便重复选择同一文件仍能触发 change。
  if (fileInput.value) fileInput.value.value = ''
}

const handleDrop = async (e: DragEvent) => {
  dragActive.value = false
  const file = e.dataTransfer?.files?.[0]
  if (file) await loadFile(file)
}
</script>
