import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  importKiroRsCredentials: vi.fn(),
  importToken: vi.fn()
}))

vi.mock('@/api/admin/kiro', () => ({
  importKiroRsCredentials: mocks.importKiroRsCredentials,
  importToken: mocks.importToken
}))

import {
  buildKiroEntryName,
  buildTokenListPayload,
  useKiroCredentialImport
} from '../useKiroCredentialImport'

const entry = (over: Record<string, unknown> = {}) => ({
  account_type: 'oauth' as const,
  auth_method: 'social',
  priority: 0,
  disabled: false,
  ...over
})

describe('buildTokenListPayload', () => {
  it('把裸 refreshToken 每行包成 JSON 对象', () => {
    // 后端纯文本通道只认 ksk_ 前缀，裸 token 必须在前端转成 JSON 走解析链。
    expect(JSON.parse(buildTokenListPayload('aaa\nbbb'))).toEqual([
      { refreshToken: 'aaa' },
      { refreshToken: 'bbb' }
    ])
  })

  it('支持 token|region 并忽略空行与注释', () => {
    expect(JSON.parse(buildTokenListPayload('  aaa|us-east-2 \n\n# 注释\nbbb'))).toEqual([
      { refreshToken: 'aaa', region: 'us-east-2' },
      { refreshToken: 'bbb' }
    ])
  })
})

describe('buildKiroEntryName', () => {
  it('优先用邮箱', () => {
    expect(buildKiroEntryName(entry({ email: 'a@x.com' }) as any, 0)).toBe('a@x.com')
  })

  it('没有邮箱时用凭证指纹后 8 位', () => {
    expect(buildKiroEntryName(entry({ refresh_token: 'abcdefghij0123456789' }) as any, 0)).toBe(
      'kiro-social-23456789'
    )
  })

  it('凭证也拿不到时退回序号', () => {
    expect(buildKiroEntryName(entry() as any, 2)).toBe('kiro-social-3')
  })
})

describe('useKiroCredentialImport', () => {
  beforeEach(() => {
    mocks.importKiroRsCredentials.mockReset()
    mocks.importToken.mockReset()
  })

  it('json tab 走宽松解析并保留跳过的条目', async () => {
    mocks.importKiroRsCredentials.mockResolvedValue({
      entries: [entry({ email: 'a@x.com' })],
      skipped: [{ index: 2, reason: 'missing credential' }]
    })

    const importer = useKiroCredentialImport()
    importer.content.value = '[{"refreshToken":"x"}]'
    const count = await importer.parse('empty')

    expect(count).toBe(1)
    expect(mocks.importKiroRsCredentials).toHaveBeenCalledWith({ content: '[{"refreshToken":"x"}]' })
    expect(importer.entryNames.value).toEqual(['a@x.com'])
    expect(importer.skippedEntries.value).toHaveLength(1)
  })

  it('token tab 先把裸 token 包成 JSON 再提交', async () => {
    mocks.importKiroRsCredentials.mockResolvedValue({ entries: [entry()] })

    const importer = useKiroCredentialImport()
    importer.selectSource('token')
    importer.content.value = 'aaa'
    await importer.parse('empty')

    expect(mocks.importKiroRsCredentials).toHaveBeenCalledWith({
      content: JSON.stringify([{ refreshToken: 'aaa' }])
    })
  })

  it('kiro_ide tab 走严格解析器并补齐预览字段', async () => {
    mocks.importToken.mockResolvedValue({
      entries: [{ account_type: 'oauth', auth_method: 'social' }]
    })

    const importer = useKiroCredentialImport()
    importer.selectSource('kiro_ide')
    importer.content.value = '{"accessToken":"a"}'
    await importer.parse('empty')

    expect(mocks.importKiroRsCredentials).not.toHaveBeenCalled()
    expect(importer.entries.value[0]).toMatchObject({ priority: 0, disabled: false })
  })

  it('缺 clientId/clientSecret 的 IDE 导出要求补设备注册信息', () => {
    const importer = useKiroCredentialImport()
    importer.selectSource('kiro_ide')
    importer.content.value = JSON.stringify({ clientIdHash: 'h', refreshToken: 'r' })

    expect(importer.needsDeviceRegistration.value).toBe(true)
    // 没填就不该让用户点解析，否则建出来的账号刷不了 token。
    expect(importer.canParse.value).toBe(false)

    importer.deviceRegistrationJson.value = '{"clientId":"c","clientSecret":"s"}'
    expect(importer.canParse.value).toBe(true)
  })

  it('同样的 JSON 放在其它 tab 不触发设备注册要求', () => {
    const importer = useKiroCredentialImport()
    importer.content.value = JSON.stringify({ clientIdHash: 'h', refreshToken: 'r' })
    expect(importer.needsDeviceRegistration.value).toBe(false)
  })

  it('collectCreatable 按开关过滤禁用条目并带上编辑后的名称', async () => {
    mocks.importKiroRsCredentials.mockResolvedValue({
      entries: [entry({ email: 'a@x.com' }), entry({ email: 'b@x.com', disabled: true })]
    })

    const importer = useKiroCredentialImport()
    importer.content.value = '[]'
    await importer.parse('empty')

    importer.entryNames.value[0] = 'renamed'
    expect(importer.creatableCount.value).toBe(1)
    expect(importer.collectCreatable()).toEqual([
      { entry: importer.entries.value[0], name: 'renamed' }
    ])

    importer.skipDisabled.value = false
    expect(importer.creatableCount.value).toBe(2)
  })

  it('切换 tab 会清掉上一轮预览，避免拿旧结果建号', async () => {
    mocks.importKiroRsCredentials.mockResolvedValue({ entries: [entry()] })

    const importer = useKiroCredentialImport()
    importer.content.value = '[]'
    await importer.parse('empty')
    expect(importer.entries.value).toHaveLength(1)

    importer.selectSource('apikey')
    expect(importer.entries.value).toHaveLength(0)
  })

  it('解析不出账号时给出提示而不是静默成功', async () => {
    mocks.importKiroRsCredentials.mockResolvedValue({ entries: [] })

    const importer = useKiroCredentialImport()
    importer.content.value = '[]'
    expect(await importer.parse('empty')).toBe(0)
    expect(importer.parseError.value).toBe('empty')
  })

  it('后端报错时透出 detail 并且不留下半截预览', async () => {
    mocks.importKiroRsCredentials.mockRejectedValue({
      response: { data: { detail: 'bad format' } }
    })

    const importer = useKiroCredentialImport()
    importer.content.value = 'x'
    expect(await importer.parse('empty')).toBe(0)
    expect(importer.parseError.value).toBe('bad format')
    expect(importer.entries.value).toHaveLength(0)
    expect(importer.parsing.value).toBe(false)
  })
})
