import { ref, computed } from 'vue'
import {
  importKiroRsCredentials,
  importToken,
  type KiroRsImportEntry,
  type KiroRsSkippedEntry
} from '@/api/admin/kiro'

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
export type KiroImportSource = 'json' | 'apikey' | 'token' | 'kiro_ide'

export const KIRO_IMPORT_SOURCES: KiroImportSource[] = ['json', 'apikey', 'token', 'kiro_ide']

/**
 * Token tab：把每行裸 refreshToken 包成 JSON 对象再交给后端。
 *
 * 后端的纯文本通道只认 ksk_ 前缀（这是刻意的「全有或全无」判定，
 * 防止格式错误的 JSON 被逐行吞成垃圾密钥），所以裸 token 在这里转换，
 * 而不是去放宽后端那条判定。
 */
export function buildTokenListPayload(raw: string): string {
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

/** 为导入的账号生成可读名称：优先邮箱，其次凭证指纹后 8 位。 */
export function buildKiroEntryName(entry: KiroRsImportEntry, idx: number): string {
  if (entry.email) return String(entry.email)
  const seed = entry.refresh_token || entry.api_key || ''
  const suffix = seed.length > 8 ? seed.slice(-8) : seed
  return suffix ? `kiro-${entry.auth_method}-${suffix}` : `kiro-${entry.auth_method}-${idx + 1}`
}

/**
 * Kiro 凭证导入的解析状态机。
 *
 * 只负责「文本 → 可预览的账号条目」，不碰账号创建：
 * 创建时要套用哪些参数（分组/代理/优先级…）由调用方决定，
 * 这样同一套解析逻辑可以直接复用添加账号弹层已有的表单参数。
 */
export function useKiroCredentialImport() {
  const source = ref<KiroImportSource>('json')
  const content = ref('')
  const deviceRegistrationJson = ref('')
  const fileName = ref('')
  const parsing = ref(false)
  const parseError = ref('')
  const entries = ref<KiroRsImportEntry[]>([])
  const entryNames = ref<string[]>([])
  // 无法识别的条目：不中断导入，但必须让用户看到少了什么、为什么少。
  const skippedEntries = ref<KiroRsSkippedEntry[]>([])
  const skipDisabled = ref(true)

  // 只有严格的 Kiro IDE 导出必须是 JSON，其余三个 tab 都接受纯文本。
  const acceptAttr = computed(() =>
    source.value === 'kiro_ide' ? 'application/json,.json' : 'application/json,.json,.txt,.jsonl'
  )
  const acceptHint = computed(() =>
    source.value === 'kiro_ide' ? 'JSON (.json)' : 'JSON / JSONL / TXT'
  )

  const creatableEntries = computed(() =>
    entries.value.filter((entry) => !(skipDisabled.value && entry.disabled))
  )
  const creatableCount = computed(() => creatableEntries.value.length)

  /**
   * Kiro IDE 导出里带 clientIdHash 但缺 clientId/clientSecret 时，
   * 必须另外提供设备注册信息才能刷新 token，否则建出来的账号一次都用不了。
   * provider 只是元数据，不用来驱动表单。
   */
  const needsDeviceRegistration = computed(() => {
    if (source.value !== 'kiro_ide') return false
    try {
      const parsed = JSON.parse(content.value)
      const items = Array.isArray(parsed) ? parsed : [parsed]
      return items.some((item) => {
        if (!item || typeof item !== 'object') return false
        const value = item as Record<string, unknown>
        const authMethod = String(value.authMethod ?? value.auth_method ?? '').trim().toLowerCase()
        if (authMethod === 'api_key' || authMethod === 'api-key' || authMethod === 'apikey') {
          return false
        }
        const clientIdHash = String(value.clientIdHash ?? value.client_id_hash ?? '').trim()
        const clientId = String(value.clientId ?? value.client_id ?? '').trim()
        const clientSecret = String(value.clientSecret ?? value.client_secret ?? '').trim()
        return Boolean(clientIdHash && (!clientId || !clientSecret))
      })
    } catch {
      return false
    }
  })

  const canParse = computed(() => {
    if (parsing.value || !content.value.trim()) return false
    if (needsDeviceRegistration.value && !deviceRegistrationJson.value.trim()) return false
    return true
  })

  const resetPreview = () => {
    entries.value = []
    entryNames.value = []
    skippedEntries.value = []
    parseError.value = ''
  }

  const resetAll = () => {
    source.value = 'json'
    content.value = ''
    deviceRegistrationJson.value = ''
    fileName.value = ''
    parsing.value = false
    skipDisabled.value = true
    resetPreview()
  }

  const selectSource = (value: KiroImportSource) => {
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
      let parsed: KiroRsImportEntry[]
      if (source.value === 'kiro_ide') {
        // Kiro IDE 导出没有 endpoint/priority/disabled，补成默认值以复用同一张预览表。
        const res = await importToken({
          token_json: content.value,
          device_registration_json: deviceRegistrationJson.value.trim() || undefined
        })
        parsed = res.entries.map((e) => ({ ...e, priority: 0, disabled: false }))
      } else {
        const payload =
          source.value === 'token' ? buildTokenListPayload(content.value) : content.value
        const res = await importKiroRsCredentials({ content: payload })
        parsed = res.entries
        skippedEntries.value = res.skipped || []
      }
      entries.value = parsed
      entryNames.value = parsed.map((entry, i) => buildKiroEntryName(entry, i))
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
  const collectCreatable = (): Array<{ entry: KiroRsImportEntry; name: string }> => {
    const result: Array<{ entry: KiroRsImportEntry; name: string }> = []
    entries.value.forEach((entry, idx) => {
      if (skipDisabled.value && entry.disabled) return
      result.push({ entry, name: entryNames.value[idx] || buildKiroEntryName(entry, idx) })
    })
    return result
  }

  return {
    source,
    content,
    deviceRegistrationJson,
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
    needsDeviceRegistration,
    canParse,
    resetPreview,
    resetAll,
    selectSource,
    loadFile,
    parse,
    collectCreatable
  }
}

export type KiroCredentialImport = ReturnType<typeof useKiroCredentialImport>
