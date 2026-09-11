import { apiClient } from '../client'

export interface KiroAuthUrlResponse {
  auth_url: string
  session_id: string
  state: string
}

export interface KiroIDCAuthUrlResponse extends KiroAuthUrlResponse {
  client_id?: string
  region?: string
  start_url?: string
}

export interface KiroTokenInfo {
  access_token?: string
  refresh_token?: string
  profile_arn?: string
  expires_at?: string
  auth_method?: string
  provider?: string
  client_id?: string
  client_secret?: string
  client_id_hash?: string
  email?: string
  start_url?: string
  region?: string
  api_region?: string
  machine_id?: string
  subscription_title?: string
  token_endpoint?: string
  issuer_url?: string
  scopes?: string
  auth_url?: string
  session_id?: string
  state?: string
  [key: string]: unknown
}

export type KiroImportAccountType = 'oauth' | 'apikey'

// A validated account creation entry returned by the Kiro IDE importer.
// OAuth fields are flattened for reuse by the existing OAuth credential
// builder; API-key entries carry api_key separately and must create an
// apikey account instead of an OAuth account.
export interface KiroImportEntry extends KiroTokenInfo {
  account_type: KiroImportAccountType
  api_key?: string
}

export interface KiroImportTokenResult {
  entries: KiroImportEntry[]
}

/** kiro.rs 凭证补齐后的条目，额外带该系统特有的调度属性。 */
export interface KiroRsImportEntry extends KiroImportEntry {
  endpoint?: string
  priority: number
  disabled: boolean
}

/** 无法识别的条目：跳过而非中断整批导入，但要能在预览里逐条说明原因。 */
export interface KiroRsSkippedEntry {
  index: number
  reason: string
  sample?: string
}

export interface KiroRsImportResult {
  entries: KiroRsImportEntry[]
  skipped?: KiroRsSkippedEntry[]
}

export async function generateAuthUrl(payload: {
  proxy_id?: number
  provider?: string
}): Promise<KiroAuthUrlResponse> {
  const { data } = await apiClient.post<KiroAuthUrlResponse>('/admin/kiro/oauth/auth-url', payload)
  return data
}

export async function generateIDCAuthUrl(payload: {
  proxy_id?: number
  start_url?: string
  region?: string
}): Promise<KiroIDCAuthUrlResponse> {
  const { data } = await apiClient.post<KiroIDCAuthUrlResponse>('/admin/kiro/oauth/idc-auth-url', payload)
  return data
}

export async function exchangeCode(payload: {
  session_id: string
  state: string
  code: string
  callback_path?: string
  login_option?: string
  proxy_id?: number
}): Promise<KiroTokenInfo> {
  const { data } = await apiClient.post<KiroTokenInfo>('/admin/kiro/oauth/exchange-code', payload)
  return data
}

export async function refreshToken(payload: {
  refresh_token: string
  auth_method?: string
  provider?: string
  client_id?: string
  client_secret?: string
  start_url?: string
  region?: string
  api_region?: string
  profile_arn?: string
  token_endpoint?: string
  issuer_url?: string
  scopes?: string
  proxy_id?: number
}): Promise<KiroTokenInfo> {
  const { data } = await apiClient.post<KiroTokenInfo>('/admin/kiro/oauth/refresh-token', payload)
  return data
}

export async function importToken(payload: {
  token_json: string
  device_registration_json?: string
}): Promise<KiroImportTokenResult> {
  const { data } = await apiClient.post<KiroImportTokenResult>('/admin/kiro/oauth/import-token', payload)
  return data
}

/**
 * 解析 kiro.rs 凭证文件并返回补齐后的账号条目。
 *
 * 与 importToken 并列：那条走 Kiro IDE 导出的严格校验，
 * 本接口面向 kiro.rs 的凭证格式（只有 refreshToken、或仅 kiroApiKey）。
 */
export async function importKiroRsCredentials(payload: {
  content: string
}): Promise<KiroRsImportResult> {
  const { data } = await apiClient.post<KiroRsImportResult>('/admin/kiro/oauth/import-kiro-rs', payload)
  return data
}

export default {
  generateAuthUrl,
  generateIDCAuthUrl,
  exchangeCode,
  refreshToken,
  importToken,
  importKiroRsCredentials
}
