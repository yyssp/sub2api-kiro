import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import RemoteLoginView from '@/views/auth/RemoteLoginView.vue'

const {
  fetchRemoteInfoMock,
  loginWithAdminKeyMock,
  pushMock,
  replaceMock,
  showSuccessMock,
  showErrorMock
} = vi.hoisted(() => ({
  fetchRemoteInfoMock: vi.fn(),
  loginWithAdminKeyMock: vi.fn(),
  pushMock: vi.fn(),
  replaceMock: vi.fn(),
  showSuccessMock: vi.fn(),
  showErrorMock: vi.fn()
}))

vi.mock('vue-router', () => ({
  useRouter: () => ({ push: pushMock, replace: replaceMock })
}))

vi.mock('vue-i18n', () => ({
  createI18n: () => ({ global: { t: (key: string) => key } }),
  useI18n: () => ({ t: (key: string) => key })
}))

vi.mock('@/stores', () => ({
  useAuthStore: () => ({ loginWithAdminKey: loginWithAdminKeyMock }),
  useAppStore: () => ({ showSuccess: showSuccessMock, showError: showErrorMock })
}))

vi.mock('@/api/remote', () => ({
  fetchRemoteInfo: (...args: unknown[]) => fetchRemoteInfoMock(...args)
}))

vi.mock('@/utils/apiError', () => ({
  extractI18nErrorMessage: () => 'auth.loginFailed'
}))

function mountView() {
  return mount(RemoteLoginView, {
    global: {
      stubs: {
        AuthLayout: { template: '<div><slot /></div>' },
        Icon: true,
        transition: false
      }
    }
  })
}

describe('RemoteLoginView', () => {
  beforeEach(() => {
    fetchRemoteInfoMock.mockReset()
    loginWithAdminKeyMock.mockReset()
    pushMock.mockReset()
    replaceMock.mockReset()
    showSuccessMock.mockReset()
    showErrorMock.mockReset()
  })

  it('未启用远程代管时跳回登录页，不渲染表单', async () => {
    fetchRemoteInfoMock.mockResolvedValue(null)

    const wrapper = mountView()
    await flushPromises()

    expect(replaceMock).toHaveBeenCalledWith('/login')
    expect(wrapper.find('#admin-key').exists()).toBe(false)
  })

  it('启用后展示 Key 输入框', async () => {
    fetchRemoteInfoMock.mockResolvedValue({ enabled: true })

    const wrapper = mountView()
    await flushPromises()

    expect(replaceMock).not.toHaveBeenCalled()
    expect(wrapper.find('#admin-key').exists()).toBe(true)
    expect(wrapper.text()).toContain('auth.remoteLoginHint')
  })

  it('登录成功后固定进入管理面，不使用 redirect 参数', async () => {
    fetchRemoteInfoMock.mockResolvedValue({ enabled: true })
    loginWithAdminKeyMock.mockResolvedValue({ id: 0, role: 'admin' })

    const wrapper = mountView()
    await flushPromises()

    await wrapper.find('#admin-key').setValue('sk-admin-key')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(loginWithAdminKeyMock).toHaveBeenCalledWith('sk-admin-key')
    expect(pushMock).toHaveBeenCalledWith('/admin/dashboard')
  })

  it('Key 无效时留在本页并提示错误', async () => {
    fetchRemoteInfoMock.mockResolvedValue({ enabled: true })
    loginWithAdminKeyMock.mockRejectedValue({ status: 401, message: 'Invalid admin API key' })

    const wrapper = mountView()
    await flushPromises()

    await wrapper.find('#admin-key').setValue('wrong-key')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(pushMock).not.toHaveBeenCalled()
    expect(showErrorMock).toHaveBeenCalled()
    expect(wrapper.text()).toContain('auth.loginFailed')
  })
})
