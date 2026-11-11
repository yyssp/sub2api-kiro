/**
 * Admin Cursor API endpoints
 *
 * ⚠️ Cursor 没有标准 OAuth 授权码流程，所以这里没有 auth-url / exchange-code：
 * 凭证是用户从 Cursor 桌面端或网页 cookie 里直接拿到的 token，
 * 唯一的接入方式就是导入。
 */

import { apiClient } from '../client'
import type { Account, CreateAccountRequest } from '@/types'

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
  /** Cursor 用户 ID（uid）：token 会轮换，uid 才是同一账号的稳定身份。 */
  user_id?: string
  /** 库中已有的同一用户账号：确认导入时更新它的 token，而不是新建。 */
  existing?: CursorExistingAccount
}

/** 库中已有的同一 Cursor 用户账号。 */
export interface CursorExistingAccount {
  id: number
  name: string
  status: string
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

/** 确认导入里的一条账号；existing_account_id 非空时只更新该账号的 token。 */
export interface CursorImportAccountPayload extends CreateAccountRequest {
  existing_account_id?: number
}

export interface CursorImportConfirmResult {
  created: number
  updated: number
  success: number
  failed: number
  results: Array<{
    name: string
    id?: number
    success: boolean
    action?: 'created' | 'updated'
    error?: string
  }>
}

/**
 * 确认导入：新用户建号（后端会兑换 web token、铸造设备指纹），已有用户只更新 token，
 * 保留名称、分组、代理与调度参数。成功的账号随后在后台补齐套餐与额度，
 * activate_sand_trial 为真时同时尝试激活 $250 Sand Trial。
 */
export async function confirmCursorImport(payload: {
  accounts: CursorImportAccountPayload[]
  activate_sand_trial: boolean
}): Promise<CursorImportConfirmResult> {
  const { data } = await apiClient.post<CursorImportConfirmResult>(
    '/admin/cursor/import-accounts',
    payload
  )
  return data
}

/**
 * 替换已有 Cursor 账号的 token（编辑弹窗用）。token 接受裸 JWT、uid::JWT 或整段 cookie；
 * 必须属于同一 Cursor 用户，web token 会经账号代理兑换，设备 ID 保持不变。
 */
export async function replaceCursorCredential(
  accountId: number,
  payload: { token: string; refresh_token?: string; activate_sand_trial?: boolean }
): Promise<Account> {
  const { data } = await apiClient.post<Account>(`/admin/cursor/accounts/${accountId}/credential`, payload)
  return data
}

export default {
  importCursorCredentials,
  confirmCursorImport,
  replaceCursorCredential
}
