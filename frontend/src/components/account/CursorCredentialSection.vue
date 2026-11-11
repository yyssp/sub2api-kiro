<template>
  <div class="space-y-3 border-t border-gray-200 pt-4 dark:border-dark-600">
    <label class="input-label">{{ t('admin.accounts.cursorCredentialTitle') }}</label>

    <!-- 只读概览：凭证本身已脱敏，只展示从 JWT / 账号信息里读得到的身份与状态。 -->
    <dl class="grid grid-cols-1 gap-x-4 gap-y-1 rounded-lg bg-gray-50 p-3 text-xs dark:bg-dark-800 sm:grid-cols-2">
      <div class="flex gap-2">
        <dt class="text-gray-500 dark:text-dark-400">{{ t('admin.accounts.cursorCredentialEmail') }}</dt>
        <dd class="text-gray-800 dark:text-dark-100">{{ email || '-' }}</dd>
      </div>
      <div class="flex gap-2">
        <dt class="text-gray-500 dark:text-dark-400">{{ t('admin.accounts.cursorCredentialMembership') }}</dt>
        <dd class="text-gray-800 dark:text-dark-100">{{ membership || '-' }}</dd>
      </div>
      <div class="flex gap-2">
        <dt class="text-gray-500 dark:text-dark-400">{{ t('admin.accounts.cursorCredentialRefresh') }}</dt>
        <dd :class="hasRefreshToken ? 'text-emerald-600 dark:text-emerald-400' : 'text-amber-600 dark:text-amber-400'">
          {{ hasRefreshToken ? t('admin.accounts.cursorCredentialRefreshYes') : t('admin.accounts.cursorCredentialRefreshNo') }}
        </dd>
      </div>
      <div class="flex gap-2">
        <dt class="text-gray-500 dark:text-dark-400">{{ t('admin.accounts.cursorCredentialMachineId') }}</dt>
        <dd class="font-mono text-gray-800 dark:text-dark-100">{{ maskedMachineId }}</dd>
      </div>
      <div v-if="trialStatus" class="flex gap-2 sm:col-span-2">
        <dt class="text-gray-500 dark:text-dark-400">{{ t('admin.accounts.cursorCredentialTrial') }}</dt>
        <dd class="text-gray-800 dark:text-dark-100">
          {{ trialStatusLabel }}<span v-if="trialError" class="ml-1 text-red-500">({{ trialError }})</span>
        </dd>
      </div>
    </dl>

    <!-- 替换 token：独立提交，不随下方「保存」一起走，避免与通用字段的全对象更新混在一起。 -->
    <div class="space-y-2">
      <textarea
        v-model="token"
        rows="3"
        class="input font-mono text-xs"
        :placeholder="t('admin.accounts.cursorCredentialTokenPlaceholder')"
      ></textarea>
      <input
        v-model="refreshToken"
        type="text"
        class="input font-mono text-xs"
        :placeholder="t('admin.accounts.cursorCredentialRefreshPlaceholder')"
      />
      <label class="flex items-center gap-2 text-xs text-gray-600 dark:text-dark-300">
        <input v-model="activateSandTrial" type="checkbox" class="rounded" />
        {{ t('admin.accounts.cursorImportActivateTrial') }}
      </label>
      <p class="input-hint">{{ t('admin.accounts.cursorCredentialReplaceHint') }}</p>
      <div class="flex justify-end">
        <button type="button" class="btn btn-secondary" :disabled="!canReplace" @click="replace">
          {{ replacing ? t('common.loading') : t('admin.accounts.cursorCredentialReplace') }}
        </button>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { replaceCursorCredential } from '@/api/admin/cursor'
import { useAppStore } from '@/stores/app'
import type { Account } from '@/types'

/**
 * 编辑弹窗里的 Cursor 凭证区：查看身份 / 续期能力 / 设备 ID，替换 token。
 *
 * 替换走专用接口（后端做 token 归一化、同一用户校验、web token 兑换），
 * 不经弹窗的通用保存：通用 PUT 会把整段 cookie 原样落库，也不校验是不是同一个 Cursor 用户。
 */
const props = defineProps<{ account: Account }>()
const emit = defineEmits<{ (e: 'replaced', account: Account): void }>()

const { t } = useI18n()
const appStore = useAppStore()

const token = ref('')
const refreshToken = ref('')
const activateSandTrial = ref(false)
const replacing = ref(false)

const credentials = computed(() => (props.account.credentials || {}) as Record<string, unknown>)
const extra = computed(() => (props.account.extra || {}) as Record<string, unknown>)

const email = computed(() => String(credentials.value.email || '').trim())
const membership = computed(() => String(credentials.value.membership || '').trim())
// refresh_token 属于敏感字段，已从 credentials 剥离，存在性读 credentials_status。
const hasRefreshToken = computed(() => Boolean(props.account.credentials_status?.has_refresh_token))

/** 设备 ID 只显示首尾，够核对、不完整暴露。 */
const maskedMachineId = computed(() => {
  const id = String(credentials.value.machine_id || '').trim()
  if (!id) return '-'
  return id.length > 12 ? `${id.slice(0, 6)}…${id.slice(-4)}` : id
})

const trialStatus = computed(() => String(extra.value.cursor_sand_trial_status || '').trim())
const trialError = computed(() => String(extra.value.cursor_sand_trial_error || '').trim())
const trialStatusLabel = computed(() => {
  switch (trialStatus.value) {
    case 'activated':
      return t('admin.accounts.cursorTrialActivated')
    case 'already':
      return t('admin.accounts.cursorTrialAlready')
    case 'not_eligible':
      return t('admin.accounts.cursorTrialNotEligible')
    case 'failed':
      return t('admin.accounts.cursorTrialFailed')
    default:
      return trialStatus.value
  }
})

const canReplace = computed(() => !replacing.value && Boolean(token.value.trim()))

const replace = async () => {
  if (!canReplace.value) return
  replacing.value = true
  try {
    const updated = await replaceCursorCredential(props.account.id, {
      token: token.value.trim(),
      refresh_token: refreshToken.value.trim() || undefined,
      activate_sand_trial: activateSandTrial.value
    })
    token.value = ''
    refreshToken.value = ''
    appStore.showSuccess(t('admin.accounts.cursorCredentialReplaced'))
    emit('replaced', updated)
  } catch (error: any) {
    appStore.showError(
      error?.response?.data?.message || error?.response?.data?.detail || t('admin.accounts.cursorCredentialReplaceFailed')
    )
  } finally {
    replacing.value = false
  }
}
</script>
