import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  importCursorCredentials: vi.fn(),
  listAccounts: vi.fn()
}))

vi.mock('@/api/admin/cursor', () => ({
  importCursorCredentials: mocks.importCursorCredentials
}))

vi.mock('@/api/admin/accounts', () => ({
  list: mocks.listAccounts
}))

import {
  buildCursorEntryName,
  ensureUniqueName,
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
  it('用表单填的名称作前缀，加 token 哈希后缀', () => {
    const name = buildCursorEntryName(entry({ access_token: 'tok-a' }), 0, 'mycursor')
    expect(name).toMatch(/^mycursor_[0-9a-f]{6}$/)
  })

  // email 是账号自身的身份标识，比"这一批"的名称更精确，优先级更高。
  it('能解出 email 时直接用 email，忽略填写的名称', () => {
    expect(
      buildCursorEntryName(entry({ email: 'a@example.com', access_token: 'tok-a' }), 0, 'mycursor')
    ).toBe('a@example.com')
  })

  // 同一个 email 即同一个账户：两条都解出同一个 email 时必须落到同一个基名，
  // 后续由去重层把它显式暴露成冲突，而不是生成两条看不出关系的记录。
  it('同一个 email 的两条生成同一个基名', () => {
    const a = buildCursorEntryName(entry({ email: 'same@example.com', access_token: 't1' }), 0, 'p')
    const b = buildCursorEntryName(entry({ email: 'same@example.com', access_token: 't2' }), 1, 'p')
    expect(a).toBe(b)
  })

  it('email 为空白时按没有 email 处理', () => {
    const name = buildCursorEntryName(entry({ email: '   ', access_token: 'tok-a' }), 0, 'mycursor')
    expect(name).toMatch(/^mycursor_[0-9a-f]{6}$/)
  })

  it('没填前缀时退回 cursor', () => {
    expect(buildCursorEntryName(entry({ access_token: 'tok-a' }), 0)).toMatch(
      /^cursor_[0-9a-f]{6}$/
    )
  })

  it('前缀是纯空白时按没填处理', () => {
    expect(buildCursorEntryName(entry({ access_token: 'tok-a' }), 0, '   ')).toMatch(
      /^cursor_[0-9a-f]{6}$/
    )
  })

  // 哈希必须喂整段 token：只喂尾部等于退回签名段，同签发方的不同账号
  // 末几位经常完全一致，会批量生成撞名账号。
  it('尾部相同但整段不同的 token 后缀也不同', () => {
    const a = buildCursorEntryName(entry({ access_token: 'AAAA-common-tail' }), 0, 'p')
    const b = buildCursorEntryName(entry({ access_token: 'BBBB-common-tail' }), 1, 'p')
    expect(a).not.toBe(b)
  })

  it('同一个 token 两次生成结果一致', () => {
    const e = entry({ access_token: 'tok-a' })
    expect(buildCursorEntryName(e, 0, 'p')).toBe(buildCursorEntryName(e, 9, 'p'))
  })

  it('连 token 都没有时才退回序号', () => {
    expect(buildCursorEntryName(entry({ access_token: '', session: '' }), 2, 'p')).toBe('p-3')
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
    // 默认库里没有账号：需要验证避让的用例自行覆盖返回值。
    mocks.listAccounts.mockReset()
    mocks.listAccounts.mockResolvedValue({ items: [], total: 0, page: 1, page_size: 20, pages: 0 })
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
      entries: [
        entry({ email: 'a@example.com', access_token: 'tok-a' }),
        entry({ access_token: 'tok-b' })
      ]
    })
    const importer = useCursorCredentialImport()
    importer.content.value = 'token-a\ntoken-b'

    const count = await importer.parse('empty', 'mycursor')

    expect(count).toBe(2)
    expect(mocks.importCursorCredentials).toHaveBeenCalledWith({ content: 'token-a\ntoken-b' })
    // 有 email 的用 email；没有的才用前缀 + 哈希。
    const [n1, n2] = importer.entryNames.value
    expect(n1).toBe('a@example.com')
    expect(n2).toMatch(/^mycursor_[0-9a-f]{6}$/)
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

describe('ensureUniqueName', () => {
  it('不冲突时原样返回', () => {
    expect(ensureUniqueName('a@example.com', new Set())).toBe('a@example.com')
  })

  it('冲突时追加序号，并持续递增直到不冲突', () => {
    const taken = new Set(['a@example.com', 'a@example.com-2'])
    expect(ensureUniqueName('a@example.com', taken)).toBe('a@example.com-3')
  })

  it('生成的名字会写回 taken，避免同批内再次撞名', () => {
    const taken = new Set<string>()
    expect(ensureUniqueName('dup', taken)).toBe('dup')
    expect(ensureUniqueName('dup', taken)).toBe('dup-2')
    expect(ensureUniqueName('dup', taken)).toBe('dup-3')
  })

  it('空名兜底成 cursor', () => {
    expect(ensureUniqueName('   ', new Set())).toBe('cursor')
  })
})

describe('useCursorCredentialImport 账号名去重', () => {
  beforeEach(() => {
    mocks.importCursorCredentials.mockReset()
    mocks.listAccounts.mockReset()
  })

  // accounts.name 没有唯一约束，重名不会被后端拒绝，只会建出一批
  // 肉眼无法区分的同名账号，所以必须在生成侧避让。
  it('避开库里已存在的账号名', async () => {
    const e = entry({ access_token: 'tok-a' })
    // 先算出本来会生成的名字，再把它塞进"库里已存在"，构造出必然的冲突。
    const collide = buildCursorEntryName(e, 0, 'mycursor')
    mocks.listAccounts.mockResolvedValue({ items: [{ name: collide }] })
    mocks.importCursorCredentials.mockResolvedValue({ entries: [e] })

    const importer = useCursorCredentialImport()
    await importer.parse('empty', 'mycursor')

    expect(importer.entryNames.value).toEqual([`${collide}-2`])
  })

  it('同一批导入内部互相重名也要避开', async () => {
    mocks.listAccounts.mockResolvedValue({ items: [] })
    // 同一个 token 出现两次 -> 哈希相同 -> 批内必然撞名。
    mocks.importCursorCredentials.mockResolvedValue({
      entries: [entry({ access_token: 'dup' }), entry({ access_token: 'dup' })]
    })

    const importer = useCursorCredentialImport()
    await importer.parse('empty', 'mycursor')

    const base = buildCursorEntryName(entry({ access_token: 'dup' }), 0, 'mycursor')
    expect(importer.entryNames.value).toEqual([base, `${base}-2`])
  })

  // 软删除账号（deleted_at 非空）在界面上根本看不见，名字必须可以被重新用上，
  // 否则用户删掉一个账号再重新导入，会莫名其妙拿到 `xxx-2`。
  // 列表接口本身已过滤 deleted_at，这里锁住"不要自作聪明去查全量"这个前提。
  it('只避让接口返回的存量账号，软删除的名字可以复用', async () => {
    const soft = entry({ access_token: 'tok-soft' })
    const live = entry({ access_token: 'tok-live' })
    const softName = buildCursorEntryName(soft, 0, 'p')
    const liveName = buildCursorEntryName(live, 1, 'p')

    // 接口返回的就是未删除账号；被软删的那个名字不在其中，所以可以原样复用。
    mocks.listAccounts.mockResolvedValue({ items: [{ name: liveName }] })
    mocks.importCursorCredentials.mockResolvedValue({ entries: [soft, live] })

    const importer = useCursorCredentialImport()
    await importer.parse('empty', 'p')

    expect(importer.entryNames.value).toEqual([softName, `${liveName}-2`])
  })

  // 只拉第一页的话，第二页上的存量名字会被当成"没人用"，
  // 于是正大光明地生成一个与现存账号同名的账号。
  it('翻完所有分页，第二页的存量名字同样要避让', async () => {
    const e = entry({ access_token: 'tok-a' })
    // 冲突名只出现在第二页：只拉第一页就会漏掉它。
    const collide = buildCursorEntryName(e, 0, 'p')
    mocks.listAccounts.mockImplementation((page: number) =>
      Promise.resolve(
        page === 1
          ? { items: [{ name: 'other-1' }], total: 2 }
          : { items: [{ name: collide }], total: 2 }
      )
    )
    mocks.importCursorCredentials.mockResolvedValue({ entries: [e] })

    const importer = useCursorCredentialImport()
    await importer.parse('empty', 'p')

    expect(mocks.listAccounts).toHaveBeenCalledTimes(2)
    expect(importer.entryNames.value).toEqual([`${collide}-2`])
  })

  // 翻页中途失败时，已取回的那部分仍要生效：能避让多少是多少。
  it('翻页中途失败仍沿用已取回的名字', async () => {
    const e = entry({ access_token: 'tok-a' })
    const collide = buildCursorEntryName(e, 0, 'p')
    mocks.listAccounts.mockImplementation((page: number) =>
      page === 1
        ? Promise.resolve({ items: [{ name: collide }], total: 2 })
        : Promise.reject(new Error('boom'))
    )
    mocks.importCursorCredentials.mockResolvedValue({ entries: [e] })

    const importer = useCursorCredentialImport()
    const count = await importer.parse('empty', 'p')

    expect(count).toBe(1)
    expect(importer.entryNames.value).toEqual([`${collide}-2`])
    expect(importer.parseError.value).toBe('')
  })

  // 拉取存量名字只是体验优化，失败不该把整个导入流程卡死。
  it('拉取存量账号失败时仍能完成解析', async () => {
    const e = entry({ access_token: 'tok-a' })
    mocks.listAccounts.mockRejectedValue(new Error('network down'))
    mocks.importCursorCredentials.mockResolvedValue({ entries: [e] })

    const importer = useCursorCredentialImport()
    const count = await importer.parse('empty', 'p')

    expect(count).toBe(1)
    expect(importer.entryNames.value).toEqual([buildCursorEntryName(e, 0, 'p')])
    expect(importer.parseError.value).toBe('')
  })
})
