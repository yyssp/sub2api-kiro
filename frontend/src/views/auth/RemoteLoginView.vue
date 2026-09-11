<template>
  <AuthLayout>
    <!--
      远程代管登录页。

      刻意与 /login 完全独立：本部署自身的账号体系（账密 / OAuth / Passkey）
      不在此页出现，/login 也不引用此页，因此普通用户不会偶然进入。
      入口地址需由部署方自行告知使用者。
    -->
    <div v-if="probing" class="py-12 text-center">
      <Icon name="refresh" size="lg" class="mx-auto animate-spin text-gray-400 dark:text-dark-500" />
    </div>

    <div v-else class="space-y-6">
      <div class="text-center">
        <h2 class="text-2xl font-bold text-gray-900 dark:text-white">
          {{ t('auth.remoteLoginTitle') }}
        </h2>
        <p class="mt-2 text-sm text-gray-500 dark:text-dark-400">
          {{ t('auth.remoteLoginHint') }}
        </p>
      </div>

      <form @submit.prevent="handleSubmit" class="space-y-5">
        <div>
          <label for="admin-key" class="input-label">
            {{ t('auth.adminKeyLabel') }}
          </label>
          <div class="relative">
            <div class="pointer-events-none absolute inset-y-0 left-0 flex items-center pl-3.5">
              <Icon name="key" size="md" class="text-gray-400 dark:text-dark-500" />
            </div>
            <input
              id="admin-key"
              v-model="adminKey"
              :type="showKey ? 'text' : 'password'"
              required
              autofocus
              autocomplete="off"
              :disabled="isLoading"
              class="input pl-11 pr-11"
              :placeholder="t('auth.adminKeyPlaceholder')"
            />
            <button
              type="button"
              @click="showKey = !showKey"
              :disabled="isLoading"
              class="absolute inset-y-0 right-0 flex items-center pr-3.5 text-gray-400 transition-colors hover:text-gray-600 dark:hover:text-dark-300"
            >
              <Icon v-if="showKey" name="eyeOff" size="md" />
              <Icon v-else name="eye" size="md" />
            </button>
          </div>
        </div>

        <p v-if="errorMessage" class="text-sm text-red-600 dark:text-red-400">
          {{ errorMessage }}
        </p>

        <button
          type="submit"
          :disabled="isLoading || !adminKey.trim()"
          class="btn btn-primary w-full"
        >
          <Icon v-if="isLoading" name="refresh" size="md" class="mr-2 animate-spin" />
          <Icon v-else name="login" size="md" class="mr-2" />
          {{ isLoading ? t('auth.signingIn') : t('auth.signIn') }}
        </button>
      </form>
    </div>
  </AuthLayout>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { AuthLayout } from '@/components/layout'
import Icon from '@/components/icons/Icon.vue'
import { useAuthStore, useAppStore } from '@/stores'
import { fetchRemoteInfo } from '@/api/remote'
import { extractI18nErrorMessage } from '@/utils/apiError'

const { t } = useI18n()
const router = useRouter()
const authStore = useAuthStore()
const appStore = useAppStore()

const probing = ref<boolean>(true)
const isLoading = ref<boolean>(false)
const adminKey = ref<string>('')
const showKey = ref<boolean>(false)
const errorMessage = ref<string>('')

/**
 * 未启用远程代管时本页不应存在：跳回首页，避免暴露本部署是否具备该能力。
 */
onMounted(async () => {
  const info = await fetchRemoteInfo()
  if (!info) {
    await router.replace('/login')
    return
  }
  probing.value = false
})

/**
 * 远程会话只能访问被转发的管理面接口，因此固定进入 /admin/dashboard，
 * 不接受 redirect 参数——它可能指向本部署的用户页面，远程会话无权访问，
 * 会被路由守卫立刻弹回，形成登录成功却又回到登录页的循环。
 */
async function handleSubmit(): Promise<void> {
  const key = adminKey.value.trim()
  if (!key || isLoading.value) {
    return
  }

  errorMessage.value = ''
  isLoading.value = true
  try {
    await authStore.loginWithAdminKey(key)
    appStore.showSuccess(t('auth.loginSuccess'))
    await router.push('/admin/dashboard')
  } catch (error: unknown) {
    errorMessage.value = extractI18nErrorMessage(error, t, 'auth.errors', t('auth.loginFailed'))
    appStore.showError(errorMessage.value)
  } finally {
    isLoading.value = false
    adminKey.value = ''
  }
}
</script>
