import { ref, computed } from 'vue'
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

/** 为导入的账号生成可读名称：优先邮箱，其次 token 后 8 位。 */
export function buildCursorEntryName(entry: CursorImportEntry, idx: number): string {
  if (entry.email) return String(entry.email)
  const seed = entry.access_token || ''
  const suffix = seed.length > 8 ? seed.slice(-8) : seed
  return suffix ? `cursor-${suffix}` : `cursor-${idx + 1}`
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
   * @returns 解析出的条目数；0 表示没有可导入的内容（原因见 parseError）
   */
  const parse = async (emptyMessage: string): Promise<number> => {
    parsing.value = true
    parseError.value = ''
    skippedEntries.value = []
    try {
      const res = await importCursorCredentials({ content: content.value })
      const parsed = res.entries || []
      entries.value = parsed
      entryNames.value = parsed.map((entry, i) => buildCursorEntryName(entry, i))
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

  /** 预览里被勾选保留的条目，连同用户可能改过的名称一起交给调用方。 */
  const collectCreatable = (): Array<{ entry: CursorImportEntry; name: string }> => {
    const result: Array<{ entry: CursorImportEntry; name: string }> = []
    entries.value.forEach((entry, idx) => {
      if (skipDisabled.value && entry.disabled) return
      result.push({ entry, name: entryNames.value[idx] || buildCursorEntryName(entry, idx) })
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
