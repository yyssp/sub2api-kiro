import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  importCursorCredentials: vi.fn()
}))

vi.mock('@/api/admin/cursor', () => ({
  importCursorCredentials: mocks.importCursorCredentials
}))

import {
  buildCursorEntryName,
  isShortLivedEntry,
  CURSOR_IMPORT_SOURCES,
  useCursorCredentialImport
} from '../useCursorCredentialImport'
import type { CursorImportEntry } from '@/api/admin/cursor'

const entry = (over: Partial<CursorImportEntry> = {}): CursorImportEntry => ({
  access_token: 'header.payload.sig',
  token_type: 'session',
  disabled: false,
  ...over
})

describe('buildCursorEntryName', () => {
  it('优先用邮箱', () => {
    expect(buildCursorEntryName(entry({ email: 'a@example.com' }), 0)).toBe('a@example.com')
  })

  it('无邮箱时用 token 后 8 位', () => {
    expect(buildCursorEntryName(entry({ access_token: 'abcdefghijklmnop' }), 0)).toBe(
      'cursor-ijklmnop'
    )
  })

  it('token 也缺失时退回序号', () => {
    expect(buildCursorEntryName(entry({ access_token: '' }), 2)).toBe('cursor-3')
  })
})

describe('isShortLivedEntry', () => {
  it('web token 是短命的', () => {
    expect(isShortLivedEntry(entry({ token_type: 'web' }))).toBe(true)
  })

  it('session token 不是', () => {
    expect(isShortLivedEntry(entry({ token_type: 'session' }))).toBe(false)
  })
})

describe('useCursorCredentialImport', () => {
  beforeEach(() => {
    mocks.importCursorCredentials.mockReset()
  })

  it('只有两个 tab，默认是 token 文本', () => {
    // Cursor 的凭证只有「一串 token」与「一份导出 JSON」两种形态，
    // 没有 Kiro 那种严格/宽松互斥的解析路径。
    expect(CURSOR_IMPORT_SOURCES).toEqual(['token', 'json'])
    expect(useCursorCredentialImport().source.value).toBe('token')
  })

  it('内容为空时不能解析', () => {
    const importer = useCursorCredentialImport()
    expect(importer.canParse.value).toBe(false)
    importer.content.value = 'eyJ.abc.sig'
    expect(importer.canParse.value).toBe(true)
  })

  it('解析成功后填充预览与名称', async () => {
    mocks.importCursorCredentials.mockResolvedValue({
      entries: [entry({ email: 'a@example.com' }), entry({ access_token: 'xxxxxxxxyyyyyyyy' })]
    })
    const importer = useCursorCredentialImport()
    importer.content.value = 'token-a\ntoken-b'

    const count = await importer.parse('empty')

    expect(count).toBe(2)
    expect(mocks.importCursorCredentials).toHaveBeenCalledWith({ content: 'token-a\ntoken-b' })
    expect(importer.entryNames.value).toEqual(['a@example.com', 'cursor-yyyyyyyy'])
    expect(importer.parseError.value).toBe('')
  })

  it('解析出 0 条时给出空提示', async () => {
    mocks.importCursorCredentials.mockResolvedValue({ entries: [] })
    const importer = useCursorCredentialImport()
    importer.content.value = 'x'

    expect(await importer.parse('没有解析到账号')).toBe(0)
    expect(importer.parseError.value).toBe('没有解析到账号')
  })

  it('后端报错时取 detail 作为错误信息', async () => {
    mocks.importCursorCredentials.mockRejectedValue({
      response: { data: { detail: '解析 Cursor 凭证失败: 凭证内容为空' } }
    })
    const importer = useCursorCredentialImport()
    importer.content.value = 'x'

    expect(await importer.parse('empty')).toBe(0)
    expect(importer.parseError.value).toContain('凭证内容为空')
    // 失败后不能把 parsing 卡在 true，否则按钮永久禁用。
    expect(importer.parsing.value).toBe(false)
  })

  it('保留后端返回的 skipped 条目', async () => {
    mocks.importCursorCredentials.mockResolvedValue({
      entries: [entry()],
      skipped: [{ index: 2, reason: '不是合法的 JWT' }]
    })
    const importer = useCursorCredentialImport()
    importer.content.value = 'x'
    await importer.parse('empty')

    // 坏条目必须能展示出来：静默丢弃会让用户以为全部导入成功。
    expect(importer.skippedEntries.value).toHaveLength(1)
    expect(importer.skippedEntries.value[0].reason).toBe('不是合法的 JWT')
  })

  it('skipDisabled 控制可创建条目', async () => {
    mocks.importCursorCredentials.mockResolvedValue({
      entries: [entry(), entry({ disabled: true })]
    })
    const importer = useCursorCredentialImport()
    importer.content.value = 'x'
    await importer.parse('empty')

    expect(importer.creatableCount.value).toBe(1)
    importer.skipDisabled.value = false
    expect(importer.creatableCount.value).toBe(2)
  })

  it('统计短命 web token 数量', async () => {
    mocks.importCursorCredentials.mockResolvedValue({
      entries: [entry({ token_type: 'web' }), entry({ token_type: 'session' })]
    })
    const importer = useCursorCredentialImport()
    importer.content.value = 'x'
    await importer.parse('empty')

    expect(importer.shortLivedCount.value).toBe(1)
  })

  it('collectCreatable 带上用户改过的名称', async () => {
    mocks.importCursorCredentials.mockResolvedValue({
      entries: [entry({ email: 'a@example.com' }), entry({ disabled: true })]
    })
    const importer = useCursorCredentialImport()
    importer.content.value = 'x'
    await importer.parse('empty')

    importer.entryNames.value[0] = '我改的名字'
    const collected = importer.collectCreatable()

    expect(collected).toHaveLength(1)
    expect(collected[0].name).toBe('我改的名字')
  })

  it('切换 tab 会清空预览但保留内容', async () => {
    mocks.importCursorCredentials.mockResolvedValue({ entries: [entry()] })
    const importer = useCursorCredentialImport()
    importer.content.value = 'x'
    await importer.parse('empty')
    expect(importer.entries.value).toHaveLength(1)

    importer.selectSource('json')

    expect(importer.entries.value).toHaveLength(0)
    // 内容保留：同一份数据换个 tab 解析是常见操作。
    expect(importer.content.value).toBe('x')
  })

  it('resetAll 清空全部状态', async () => {
    mocks.importCursorCredentials.mockResolvedValue({ entries: [entry()] })
    const importer = useCursorCredentialImport()
    importer.source.value = 'json'
    importer.content.value = 'x'
    importer.fileName.value = 'a.json'
    await importer.parse('empty')

    importer.resetAll()

    expect(importer.source.value).toBe('token')
    expect(importer.content.value).toBe('')
    expect(importer.fileName.value).toBe('')
    expect(importer.entries.value).toHaveLength(0)
    expect(importer.skipDisabled.value).toBe(true)
  })
})
