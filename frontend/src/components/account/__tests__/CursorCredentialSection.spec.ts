import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

const mocks = vi.hoisted(() => ({
  replaceCursorCredential: vi.fn(),
  showSuccess: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/api/admin/cursor', () => ({
  replaceCursorCredential: mocks.replaceCursorCredential
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showSuccess: mocks.showSuccess, showError: mocks.showError })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import CursorCredentialSection from '../CursorCredentialSection.vue'
import type { Account } from '@/types'

const account = (over: Partial<Account> = {}): Account =>
  ({
    id: 8,
    name: 'c',
    platform: 'cursor',
    type: 'oauth',
    credentials: { email: 'a@x.test', membership: 'pro', machine_id: '0123456789abcdef0123' },
    credentials_status: { has_refresh_token: true },
    extra: {},
    ...over
  }) as unknown as Account

describe('CursorCredentialSection', () => {
  beforeEach(() => {
    mocks.replaceCursorCredential.mockReset()
    mocks.showSuccess.mockReset()
    mocks.showError.mockReset()
  })

  it('展示邮箱、套餐、续期能力，设备 ID 只露首尾', () => {
    const text = mount(CursorCredentialSection, { props: { account: account() } }).text()
    expect(text).toContain('a@x.test')
    expect(text).toContain('pro')
    expect(text).toContain('admin.accounts.cursorCredentialRefreshYes')
    expect(text).toContain('012345…0123')
    expect(text).not.toContain('0123456789abcdef0123')
  })

  it('没有 refresh token 时提示到期需重新导入', () => {
    const text = mount(CursorCredentialSection, {
      props: { account: account({ credentials_status: {} }) }
    }).text()
    expect(text).toContain('admin.accounts.cursorCredentialRefreshNo')
  })

  it('替换 token 走专用接口，成功后回传更新后的账号并清空输入', async () => {
    const updated = account({ name: 'updated' })
    mocks.replaceCursorCredential.mockResolvedValue(updated)
    const wrapper = mount(CursorCredentialSection, { props: { account: account() } })

    const button = wrapper.find('button')
    expect(button.attributes('disabled')).toBeDefined()

    await wrapper.find('textarea').setValue('  user_A::header.payload.sig  ')
    await wrapper.find('input[type="text"]').setValue('rt-1')
    await wrapper.find('input[type="checkbox"]').setValue(true)
    await wrapper.find('button').trigger('click')
    await flushPromises()

    expect(mocks.replaceCursorCredential).toHaveBeenCalledWith(8, {
      token: 'user_A::header.payload.sig',
      refresh_token: 'rt-1',
      activate_sand_trial: true
    })
    expect(wrapper.emitted('replaced')?.[0]).toEqual([updated])
    expect((wrapper.find('textarea').element as HTMLTextAreaElement).value).toBe('')
    expect(mocks.showSuccess).toHaveBeenCalled()
  })

  it('后端拒绝（如不是同一 Cursor 用户）时展示后端给出的原因', async () => {
    mocks.replaceCursorCredential.mockRejectedValue({
      response: { data: { message: '凭证与目标账号不属于同一 Cursor 用户' } }
    })
    const wrapper = mount(CursorCredentialSection, { props: { account: account() } })
    await wrapper.find('textarea').setValue('other.token.sig')
    await wrapper.find('button').trigger('click')
    await flushPromises()

    expect(mocks.showError).toHaveBeenCalledWith('凭证与目标账号不属于同一 Cursor 用户')
    expect(wrapper.emitted('replaced')).toBeUndefined()
  })

  it('展示最近一次 Sand Trial 激活结果', () => {
    const text = mount(CursorCredentialSection, {
      props: { account: account({ extra: { cursor_sand_trial_status: 'failed', cursor_sand_trial_error: 'HTTP 500' } }) }
    }).text()
    expect(text).toContain('admin.accounts.cursorTrialFailed')
    expect(text).toContain('HTTP 500')
  })
})
