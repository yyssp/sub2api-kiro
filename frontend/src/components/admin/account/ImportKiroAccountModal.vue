<template>
  <BaseDialog
    :show="show"
    :title="t('admin.accounts.kiroImportTitle')"
    width="wide"
    close-on-click-outside
    @close="handleClose"
  >
    <div class="space-y-4">
      <!-- 按数据形态分 tab：用户拿到的是一坨数据，他知道那是 JSON 还是一串 ksk，
           但未必分得清那是「KAM 导出」还是「kiro.rs 凭证」，所以不按来源分。 -->
      <div>
        <div
          class="flex gap-1 rounded-lg bg-gray-100 p-1 dark:bg-dark-800"
          role="tablist"
        >
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
        <input ref="fileInput" type="file" class="hidden" :accept="acceptAttr" @change="handleFileChange" />
      </div>

      <!-- 内容粘贴 -->
      <div>
        <label class="input-label">{{ t('admin.accounts.kiroImportContent') }}</label>
        <textarea
          v-model="content"
          rows="8"
          class="input font-mono text-xs"
          :placeholder="currentPlaceholder"
          @input="resetPreview"
        ></textarea>
      </div>

      <div v-if="parseError" class="rounded-lg border border-red-200 bg-red-50 p-3 text-sm text-red-600 dark:border-red-800 dark:bg-red-900/20 dark:text-red-400">
        {{ parseError }}
      </div>

      <!-- 解析预览：导入前先确认补齐结果，避免脏数据直接落库。 -->
      <div v-if="entries.length" class="space-y-3 rounded-xl border border-gray-200 p-4 dark:border-dark-700">
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
                <th class="px-2 py-1 font-medium">{{ t('admin.accounts.kiroImportColPriority') }}</th>
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
                <td class="px-2 py-1">{{ entry.priority }}</td>
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
      </div>

      <!-- 跳过的条目：坏数据不该中断整批导入，但也不能静默消失。 -->
      <div
        v-if="skippedEntries.length"
        class="space-y-2 rounded-xl border border-amber-200 bg-amber-50 p-4 dark:border-amber-800 dark:bg-amber-900/20"
      >
        <div class="text-sm font-medium text-amber-800 dark:text-amber-300">
          {{ t('admin.accounts.kiroImportSkipped', { count: skippedEntries.length }) }}
        </div>
        <div class="max-h-40 overflow-auto space-y-1">
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

      <div v-if="importResult" class="space-y-2 rounded-xl border border-gray-200 p-4 dark:border-dark-700">
        <div class="text-sm font-medium text-gray-900 dark:text-white">
          {{ t('admin.accounts.kiroImportResult') }}
        </div>
        <div class="text-sm text-gray-700 dark:text-dark-300">
          {{ t('admin.accounts.kiroImportResultSummary', importResult) }}
        </div>
        <div v-if="importErrors.length" class="mt-2 max-h-40 overflow-auto rounded-lg bg-gray-50 p-3 font-mono text-xs dark:bg-dark-800">
          <div v-for="(err, i) in importErrors" :key="i" class="text-red-600 dark:text-red-400">{{ err }}</div>
        </div>
      </div>
    </div>

    <template #footer>
      <button type="button" class="btn btn-secondary" @click="handleClose">
        {{ t('common.cancel') }}
      </button>
      <button
        v-if="!entries.length"
        type="button"
        class="btn btn-primary"
        :disabled="!content.trim() || parsing"
        @click="handleParse"
      >
        {{ parsing ? t('common.loading') : t('admin.accounts.kiroImportParse') }}
      </button>
      <button
        v-else
        type="button"
        class="btn btn-primary"
        :disabled="importing || creatableCount === 0"
        @click="handleImport"
      >
        {{ importing ? t('common.loading') : t('admin.accounts.kiroImportConfirm', { count: creatableCount }) }}
      </button>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import {
  importKiroRsCredentials,
  importToken,
  type KiroRsImportEntry,
  type KiroRsSkippedEntry
} from '@/api/admin/kiro'
import { batchCreate } from '@/api/admin/accounts'
import type { CreateAccountRequest } from '@/types'

const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{ close: []; imported: [] }>()

const { t } = useI18n()

/**
 * 导入 tab，按**数据形态**划分而非按来源：
 * - json:     一切 JSON 系（单对象/数组/JSONL/accounts 包装/KAM 新旧格式），宽松解析
 * - apikey:   ksk_xxx|region 纯文本清单
 * - token:    裸 refreshToken 文本，每行一条
 * - kiro_ide: Kiro IDE 导出，走**严格**解析器，与上面三者互斥
 *
 * 前三个 tab 共用后端宽松解析链，差别只在给用户的提示与文件类型；
 * kiro_ide 必须单独保留：放宽它会让「任意 OAuth JSON 被当成 Kiro 账号」的防护失效。
 */
type ImportSource = 'json' | 'apikey' | 'token' | 'kiro_ide'

const source = ref<ImportSource>('json')
const content = ref('')
const fileName = ref('')
const dragActive = ref(false)
const fileInput = ref<HTMLInputElement | null>(null)
const parsing = ref(false)
const importing = ref(false)
const parseError = ref('')
const entries = ref<KiroRsImportEntry[]>([])
const entryNames = ref<string[]>([])
// 无法识别的条目：不中断导入，但必须让用户看到少了什么、为什么少。
const skippedEntries = ref<KiroRsSkippedEntry[]>([])
const skipDisabled = ref(true)
const importResult = ref<{ success: number; failed: number } | null>(null)
const importErrors = ref<string[]>([])

const sourceOptions = computed(() => [
  { value: 'json' as const, label: t('admin.accounts.kiroImportTabJson') },
  { value: 'apikey' as const, label: t('admin.accounts.kiroImportTabApiKey') },
  { value: 'token' as const, label: t('admin.accounts.kiroImportTabToken') },
  { value: 'kiro_ide' as const, label: t('admin.accounts.kiroImportTabKiroIde') }
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

// 只有严格的 Kiro IDE 导出必须是 JSON，其余三个 tab 都接受纯文本。
const acceptAttr = computed(() =>
  source.value === 'kiro_ide' ? 'application/json,.json' : 'application/json,.json,.txt,.jsonl'
)
const acceptHint = computed(() =>
  source.value === 'kiro_ide' ? 'JSON (.json)' : 'JSON / JSONL / TXT'
)

const creatableCount = computed(
  () => entries.value.filter((e) => !(skipDisabled.value && e.disabled)).length
)

const selectSource = (value: ImportSource) => {
  if (source.value === value) return
  source.value = value
  resetPreview()
}

const resetPreview = () => {
  entries.value = []
  entryNames.value = []
  skippedEntries.value = []
  parseError.value = ''
  importResult.value = null
  importErrors.value = []
}

const resetAll = () => {
  content.value = ''
  fileName.value = ''
  dragActive.value = false
  resetPreview()
}

watch(
  () => props.show,
  (visible) => {
    if (!visible) resetAll()
  }
)

const openFilePicker = () => fileInput.value?.click()

const readFile = async (file: File) => {
  fileName.value = file.name
  content.value = await file.text()
  resetPreview()
}

const handleFileChange = async (e: Event) => {
  const file = (e.target as HTMLInputElement).files?.[0]
  if (file) await readFile(file)
  // 清空以便重复选择同一文件仍能触发 change。
  if (fileInput.value) fileInput.value.value = ''
}

const handleDrop = async (e: DragEvent) => {
  dragActive.value = false
  const file = e.dataTransfer?.files?.[0]
  if (file) await readFile(file)
}

/** 为导入的账号生成可读名称：优先邮箱，其次凭证指纹后 8 位。 */
const buildEntryName = (entry: KiroRsImportEntry, idx: number): string => {
  if (entry.email) return entry.email
  const seed = entry.refresh_token || entry.api_key || ''
  const suffix = seed.length > 8 ? seed.slice(-8) : seed
  return suffix ? `kiro-${entry.auth_method}-${suffix}` : `kiro-${entry.auth_method}-${idx + 1}`
}

/**
 * Token tab：把每行裸 refreshToken 包成 JSON 对象再交给后端。
 *
 * 后端的纯文本通道只认 ksk_ 前缀（这是刻意的「全有或全无」判定，
 * 防止格式错误的 JSON 被逐行吞成垃圾密钥），所以裸 token 在这里转换，
 * 而不是去放宽后端那条判定。
 */
const buildTokenListPayload = (raw: string): string => {
  const items = raw
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter((line) => line && !line.startsWith('#'))
    .map((line) => {
      // 同样支持 token|region 的写法，与 ksk 清单保持一致。
      const [token, region] = line.split('|', 2)
      const item: Record<string, string> = { refreshToken: token.trim() }
      if (region?.trim()) item.region = region.trim()
      return item
    })
  return JSON.stringify(items)
}

const handleParse = async () => {
  parsing.value = true
  parseError.value = ''
  skippedEntries.value = []
  try {
    let parsed: KiroRsImportEntry[]
    if (source.value === 'kiro_ide') {
      // Kiro IDE 导出没有 endpoint/priority/disabled，补成默认值以复用同一张预览表。
      const res = await importToken({ token_json: content.value })
      parsed = res.entries.map((e) => ({ ...e, priority: 0, disabled: false }))
    } else {
      const payload =
        source.value === 'token' ? buildTokenListPayload(content.value) : content.value
      const res = await importKiroRsCredentials({ content: payload })
      parsed = res.entries
      skippedEntries.value = res.skipped || []
    }
    entries.value = parsed
    entryNames.value = parsed.map((e, i) => buildEntryName(e, i))
    if (!parsed.length) parseError.value = t('admin.accounts.kiroImportEmpty')
  } catch (err: any) {
    parseError.value = err?.message || String(err)
  } finally {
    parsing.value = false
  }
}

/** 把补齐后的条目转换成账号创建请求。 */
const buildAccountRequest = (entry: KiroRsImportEntry, name: string): CreateAccountRequest => {
  const credentials: Record<string, unknown> = {}
  const put = (key: string, value: unknown) => {
    if (value !== undefined && value !== null && value !== '') credentials[key] = value
  }

  put('auth_method', entry.auth_method)
  put('access_token', entry.access_token)
  put('refresh_token', entry.refresh_token)
  put('expires_at', entry.expires_at)
  put('client_id', entry.client_id)
  put('client_secret', entry.client_secret)
  put('client_id_hash', entry.client_id_hash)
  put('profile_arn', entry.profile_arn)
  put('region', entry.region)
  put('api_region', entry.api_region)
  put('machine_id', entry.machine_id)
  put('start_url', entry.start_url)
  put('provider', entry.provider)
  put('email', entry.email)
  put('subscription_title', entry.subscription_title)
  put('token_endpoint', entry.token_endpoint)
  put('issuer_url', entry.issuer_url)
  put('scopes', entry.scopes)
  put('endpoint', entry.endpoint)
  if (entry.account_type === 'apikey') put('api_key', entry.api_key)

  return {
    name,
    platform: 'kiro' as CreateAccountRequest['platform'],
    type: entry.account_type as CreateAccountRequest['type'],
    credentials,
    priority: entry.priority ?? 0
  }
}

const handleImport = async () => {
  importing.value = true
  importErrors.value = []
  try {
    const payload: CreateAccountRequest[] = []
    entries.value.forEach((entry, idx) => {
      if (skipDisabled.value && entry.disabled) return
      payload.push(buildAccountRequest(entry, entryNames.value[idx] || buildEntryName(entry, idx)))
    })

    const res = await batchCreate(payload)
    importResult.value = { success: res.success, failed: res.failed }
    importErrors.value = (res.results || [])
      .filter((r) => !r.success && r.error)
      .map((r) => r.error as string)

    if (res.success > 0) emit('imported')
  } catch (err: any) {
    parseError.value = err?.message || String(err)
  } finally {
    importing.value = false
  }
}

const handleClose = () => emit('close')
</script>
