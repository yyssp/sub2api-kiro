import type { Account } from '@/types'

/**
 * Kiro Token JSON 导入框的示例文本。
 *
 * 刻意不放进 i18n:JSON 示例与语言无关,而 vue-i18n 在运行时才编译消息,
 * 未转义的花括号会被当成插值语法并抛 "Invalid token in placeholder",
 * 渲染时直接炸掉整个组件树(见 i18n/__tests__/localesMessageCompile.spec.ts)。
 * 连接词由调用方用 i18n 拼接。
 */
export const KIRO_TOKEN_JSON_EXAMPLES = [
  '{"accessToken":"...","refreshToken":"...","authMethod":"social","apiRegion":"us-east-1"}',
  '{"authMethod":"api_key","kiroApiKey":"ksk_...","machineId":"..."}'
] as const

/**
 * 拼出 Token JSON 输入框的 placeholder:两个示例之间插入本地化连接词。
 */
export function kiroTokenJsonPlaceholder(orWord: string): string {
  return KIRO_TOKEN_JSON_EXAMPLES.join(`\n${orWord}\n`)
}

/**
 * 读取账号 credentials 中的 base_url(去空白)。
 */
function readBaseUrl(account: Pick<Account, 'credentials'> | null | undefined): string {
  if (!account?.credentials) return ''
  const raw = (account.credentials as Record<string, unknown>).base_url
  return typeof raw === 'string' ? raw.trim() : ''
}

/**
 * Kiro 外部中转账号:platform=kiro、type=apikey 且配置了 base_url。
 * 这类账号转发到外部 Anthropic 兼容上游({base_url}/v1/messages),
 * 不直连 AWS、无 Kiro credits,作为分组兜底/灾备。
 */
export function isKiroRelayAccount(account: Pick<Account, 'platform' | 'type' | 'credentials'> | null | undefined): boolean {
  if (!account || account.platform !== 'kiro' || account.type !== 'apikey') return false
  return readBaseUrl(account) !== ''
}

/**
 * Kiro 直连 AWS 的 API Key 账号:platform=kiro、type=apikey 且未配置 base_url。
 * 这类账号用 ksk_ 直连 q.{region}.amazonaws.com,展示 Kiro credits。
 */
export function isKiroDirectApiKeyAccount(account: Pick<Account, 'platform' | 'type' | 'credentials'> | null | undefined): boolean {
  if (!account || account.platform !== 'kiro' || account.type !== 'apikey') return false
  return readBaseUrl(account) === ''
}
