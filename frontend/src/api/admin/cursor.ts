/**
 * Admin Cursor API endpoints
 *
 * ⚠️ Cursor 没有标准 OAuth 授权码流程，所以这里没有 auth-url / exchange-code：
 * 凭证是用户从 Cursor 桌面端或网页 cookie 里直接拿到的 token，
 * 唯一的接入方式就是导入。
 */

import { apiClient } from '../client'

/** 一条解析出来的 Cursor 凭证（与后端 service.CursorImportEntry 一致）。 */
export interface CursorImportEntry {
  access_token: string
  refresh_token?: string
  session?: string
  email?: string
  machine_id?: string
  /**
   * JWT 的 type 声明：session / web / unknown。
   * web 寿命只有几小时，必须在预览里显著提示用户尽快使用。
   */
  token_type: string
  /** 取自 JWT exp（RFC3339），无法解析时为空。 */
  expires_at?: string
  note?: string
  disabled: boolean
}

/** 一条被跳过的记录及原因。 */
export interface CursorImportSkippedEntry {
  index: number
  reason: string
  sample?: string
}

export interface CursorImportResult {
  entries: CursorImportEntry[]
  /** 无法识别或重复的条目，前端需逐条展示——静默丢弃会让用户以为全部导入成功。 */
  skipped?: CursorImportSkippedEntry[]
}

/**
 * 解析粘贴的 Cursor 凭证文本，返回可预览的账号条目。
 *
 * 只解析不建号：账号创建仍走既有的批量创建接口，这样导入的账号与
 * 手工添加的账号共享同一套表单参数（分组/代理/优先级/并发…）。
 *
 * 支持的输入形态：每行一个 token 的纯文本（裸 JWT、uid::JWT、
 * 整段 WorkosCursorSessionToken cookie）、单对象 JSON、数组 JSON、JSONL。
 */
export async function importCursorCredentials(payload: {
  content: string
}): Promise<CursorImportResult> {
  const { data } = await apiClient.post<CursorImportResult>(
    '/admin/cursor/import-credentials',
    payload
  )
  return data
}

export default {
  importCursorCredentials
}
