import { ref, computed } from 'vue'
// 直接取 accounts 模块而不是 adminAPI 聚合入口：聚合入口会把全部平台的
// API 一并拉进来，让这个 composable 的测试被迫 mock 一整张 API 表。
import { list as listAccounts } from '@/api/admin/accounts'
import {
  importCursorCredentials,
  type CursorImportEntry,
  type CursorImportSkippedEntry
} from '@/api/admin/cursor'

/**
 * 导入 tab，按**数据形态**划分：
 * - token: 每行一个 token 的纯文本（裸 JWT、uid::JWT、整段 cookie 都认）
 * - json:  Cursor 账号池导出的 JSON（单对象/数组/JSONL/accounts 包装）
 *
 * 两个 tab 共用同一个后端解析链（后端会自行判别形态），差别只在给用户的
 * 提示文案与可选文件类型。这里没有 Kiro 那种「严格/宽松」两条互斥解析
 * 路径——Cursor 的凭证就是一个 JWT，没有需要额外保护的设备注册信息。
 */
export type CursorImportSource = 'token' | 'json'

export const CURSOR_IMPORT_SOURCES: CursorImportSource[] = ['token', 'json']

/**
 * 为导入的账号生成名称：**能解出 email 就用 email，否则用填写的名称作前缀**。
 *
 * email 优先于用户填的名称，是因为 email 就是这个 Cursor 账号本身的身份标识，
 * 同一个 email 即同一个账户；直接拿它当名字，账号在列表里是自解释的，
 * 也让"同一个账号被导入两次"变成一次显式的重名冲突，而不是两条看不出关系的记录。
 * 用户填的名称在这里反而是更弱的信息——它描述的是"这一批"，不是"这一个"。
 *
 * 解不出 email 时才退回填写的名称作前缀（没填则用 `cursor`），
 * 后缀优先用整段 token 的哈希，取不到 token 才用序号：
 * 哈希对同一个账号是稳定的，重复解析不会漂名字，序号则会随条目顺序变。
 *
 * ⚠️ 哈希必须喂**整段** token。JWT 尾部是签名段，同一签发方的不同账号
 * 那几位经常完全一样（实测两个不同 uid 的 token 末 8 位都是 `Mzh9.sig`），
 * 只喂尾部会批量生成互相撞名的账号。
 */
export function buildCursorEntryName(
  entry: CursorImportEntry,
  idx: number,
  prefix?: string
): string {
  const email = String(entry.email || '').trim()
  if (email) return email

  const base = String(prefix || '').trim() || 'cursor'
  const seed = String(entry.access_token || entry.session || '')
  return seed ? `${base}_${shortHash(seed)}` : `${base}-${idx + 1}`
}

/**
 * 取 token 的 6 位十六进制短哈希，用作没有任何身份信息时的账号名后缀。
 *
 * 用 FNV-1a：纯同步、不依赖 crypto.subtle（后者是 Promise 接口，
 * 塞进同步的名称生成里会把整条调用链染成 async）。这里只求"不同 token
 * 得到不同后缀"的可读标识，不是安全用途，无需抗碰撞强度。
 *
 * ⚠️ 必须喂**整段** token。只喂尾部等于退回签名段，撞名问题会原样复发。
 */
function shortHash(input: string): string {
  let hash = 0x811c9dc5
  for (let i = 0; i < input.length; i += 1) {
    hash ^= input.charCodeAt(i)
    // FNV 质数 16777619，用移位相加避免 32 位溢出丢精度。
    hash = (hash + ((hash << 1) + (hash << 4) + (hash << 7) + (hash << 8) + (hash << 24))) >>> 0
  }
  return hash.toString(16).padStart(8, '0').slice(-6)
}

/**
 * 在 taken 之外生成不重名的账号名：冲突时追加 `-2`、`-3`…
 *
 * ⚠️ accounts.name 上没有唯一约束（库里查过），重名不会被后端拒绝，
 * 只会建出一批肉眼无法区分的同名账号——排障时根本分不清是哪一个。
 * 所以去重必须在生成侧做掉。
 *
 * taken 会被就地补充：同一批导入内部的互相重名也要避开，
 * 只比对库里的存量名字是不够的。
 */
export function ensureUniqueName(base: string, taken: Set<string>): string {
  const seed = base.trim() || 'cursor'
  let candidate = seed
  let n = 2
  while (taken.has(candidate)) {
    candidate = `${seed}-${n}`
    n += 1
  }
  taken.add(candidate)
  return candidate
}

/**
 * web token 的寿命只有几小时（session token 是 60 天）。
 *
 * 建号时后端会兑换成长效 token，但如果用户粘的是已经放了一天的 web token，
 * 兑换会失败、账号建好即失效。所以预览阶段必须把这类条目标出来。
 */
export function isShortLivedEntry(entry: CursorImportEntry): boolean {
  return entry.token_type === 'web'
}

/** 每页拉多少个账号名。仅影响请求次数，不影响结果完整性。 */
const NAME_FETCH_PAGE_SIZE = 500
/** 兜底上限，防止 total 异常时无限翻页。 */
const NAME_FETCH_MAX_PAGES = 50

/**
 * 拉取库里已有的账号名，供生成阶段避让。
 *
 * ⚠️ 必须翻完所有分页。只拉第一页的话，超出页大小的那些名字会被当成"没人用"，
 * 于是生成器正大光明地造出一个与现存账号同名的账号——正是要避免的情况。
 * 这里按 total 翻页，而不是赌某个"足够大"的 page_size：页大小上限是服务端的
 * 实现细节，随时可能收紧，赌数字会在账号变多后静默退化。
 *
 * ⚠️ 不按 platform 过滤：重名的困扰是"列表里两个同名账号分不清"，
 * 与平台无关，只查 cursor 会漏掉与其它平台账号的撞名。
 *
 * ⚠️ 软删除账号不算占用：接口本身已过滤 deleted_at，这里不要绕过它去查全量。
 * 被删掉的账号在界面上根本看不见，若仍占着名字，用户删号后重新导入
 * 会莫名其妙拿到 `xxx-2`。
 *
 * 拉取失败返回空集合而不是抛错：这是一个尽力而为的体验优化，
 * 不该让一次列表请求失败把整个导入流程卡死。
 */
async function fetchExistingAccountNames(): Promise<Set<string>> {
  const names = new Set<string>()
  try {
    for (let page = 1; page <= NAME_FETCH_MAX_PAGES; page += 1) {
      // lite 模式只取列表展示字段，避免为了几个名字拉回全量凭证。
      const res = await listAccounts(page, NAME_FETCH_PAGE_SIZE, { lite: 'true' })
      const items = (res?.items || []) as Array<{ name?: string }>
      items.forEach((item) => {
        const name = String(item?.name || '').trim()
        if (name) names.add(name)
      })

      // 服务端可能把 page_size 压到更小的值，所以按"已取回条数"判断是否到底，
      // 不能假设每页正好是 NAME_FETCH_PAGE_SIZE 条。
      const total = Number(res?.total || 0)
      if (!items.length || page * items.length >= total) break
    }
    return names
  } catch {
    // 已经取回的部分仍然有用：能避让多少是多少，总好过整批不避让。
    return names
  }
}

/**
 * Cursor 凭证导入的解析状态机。
 *
 * 只负责「文本 → 可预览的账号条目」，不碰账号创建：
 * 创建时要套用哪些参数（分组/代理/优先级…）由调用方决定，
 * 这样同一套解析逻辑可以直接复用添加账号弹层已有的表单参数。
 */
export function useCursorCredentialImport() {
  const source = ref<CursorImportSource>('token')
  const content = ref('')
  const fileName = ref('')
  const parsing = ref(false)
  const parseError = ref('')
  const entries = ref<CursorImportEntry[]>([])
  const entryNames = ref<string[]>([])
  // 无法识别或重复的条目：不中断导入，但必须让用户看到少了什么、为什么少。
  const skippedEntries = ref<CursorImportSkippedEntry[]>([])
  // 是否跳过导出文件里标记为停用的条目。
  const skipDisabled = ref(true)

  const acceptAttr = computed(() =>
    source.value === 'json' ? 'application/json,.json,.jsonl' : '.txt,text/plain,.json,.jsonl'
  )
  const acceptHint = computed(() => (source.value === 'json' ? 'JSON / JSONL' : 'TXT / JSON'))

  const creatableEntries = computed(() =>
    entries.value.filter((entry) => !(skipDisabled.value && entry.disabled))
  )
  const creatableCount = computed(() => creatableEntries.value.length)

  /** 预览里有多少条是短命的 web token，供 UI 汇总提示。 */
  const shortLivedCount = computed(
    () => creatableEntries.value.filter((entry) => isShortLivedEntry(entry)).length
  )

  const canParse = computed(() => !parsing.value && Boolean(content.value.trim()))

  const resetPreview = () => {
    entries.value = []
    entryNames.value = []
    skippedEntries.value = []
    parseError.value = ''
  }

  const resetAll = () => {
    source.value = 'token'
    content.value = ''
    fileName.value = ''
    parsing.value = false
    skipDisabled.value = true
    resetPreview()
  }

  const selectSource = (value: CursorImportSource) => {
    if (source.value === value) return
    source.value = value
    resetPreview()
  }

  const loadFile = async (file: File) => {
    fileName.value = file.name
    content.value = await file.text()
    resetPreview()
  }

  /**
   * 解析当前内容并填充预览。
   * @param emptyMessage 没解析到条目时展示的提示
   * @param namePrefix 表单里填的账号名，作为生成名称的前缀；留空则用 `cursor`
   * @returns 解析出的条目数；0 表示没有可导入的内容（原因见 parseError）
   */
  const parse = async (emptyMessage: string, namePrefix?: string): Promise<number> => {
    parsing.value = true
    parseError.value = ''
    skippedEntries.value = []
    try {
      const res = await importCursorCredentials({ content: content.value })
      const parsed = res.entries || []
      entries.value = parsed
      // 存量名字取不到时按空集合处理：拉取失败不该挡住导入本身，
      // 最坏情况退化成"只保证批内不重名"，与改动前持平。
      const taken = await fetchExistingAccountNames()
      entryNames.value = parsed.map((entry, i) =>
        ensureUniqueName(buildCursorEntryName(entry, i, namePrefix), taken)
      )
      skippedEntries.value = res.skipped || []
      if (!parsed.length) parseError.value = emptyMessage
      return parsed.length
    } catch (err: any) {
      parseError.value = err?.response?.data?.detail || err?.message || String(err)
      return 0
    } finally {
      parsing.value = false
    }
  }

  /**
   * 预览里被勾选保留的条目，连同用户可能改过的名称一起交给调用方。
   *
   * namePrefix 用于兜底：用户把某行名字清空时要重新生成，此时得沿用
   * 解析时那个前缀，否则这一行会莫名其妙退回 `cursor_` 开头。
   */
  const collectCreatable = (
    namePrefix?: string
  ): Array<{ entry: CursorImportEntry; name: string }> => {
    const result: Array<{ entry: CursorImportEntry; name: string }> = []
    entries.value.forEach((entry, idx) => {
      if (skipDisabled.value && entry.disabled) return
      const name =
        String(entryNames.value[idx] || '').trim() || buildCursorEntryName(entry, idx, namePrefix)
      result.push({ entry, name })
    })
    return result
  }

  return {
    source,
    content,
    fileName,
    parsing,
    parseError,
    entries,
    entryNames,
    skippedEntries,
    skipDisabled,
    acceptAttr,
    acceptHint,
    creatableEntries,
    creatableCount,
    shortLivedCount,
    canParse,
    resetPreview,
    resetAll,
    selectSource,
    loadFile,
    parse,
    collectCreatable
  }
}

export type CursorCredentialImport = ReturnType<typeof useCursorCredentialImport>
