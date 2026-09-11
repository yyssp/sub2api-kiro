const DEFAULT_API_BASE_URL = '/api/v1'
const API_BASE_URL = normalizeAPIBaseURL(import.meta.env.VITE_API_BASE_URL)

/**
 * 远程代管入口前缀。与后端 remoteproxy.RemoteAdminPrefix 保持一致。
 */
export const REMOTE_ADMIN_PREFIX = '/remote-admin'

const LOCAL_ADMIN_PREFIX = '/admin'
const REMOTE_MODE_KEY = 'remote_proxy_mode'

/**
 * 是否处于远程代管会话。
 *
 * 本模块刻意不依赖任何其它模块（client.ts / remote.ts 都间接依赖它），
 * 因此直接读 localStorage，键名与 remote.ts 的 REMOTE_MODE_KEY 保持一致。
 */
export function isRemoteProxySession(): boolean {
  try {
    return localStorage.getItem(REMOTE_MODE_KEY) === '1'
  } catch {
    return false
  }
}

/**
 * 远程代管会话下把管理面路径改写到转发入口。
 *
 * 只改写「/admin」开头且成段匹配的路径：`/admins`、`/administrators` 之类
 * 不是管理面入口，必须原样保留。非远程会话直接返回原值。
 */
export function rewriteAdminPathForRemote(path: string): string {
  if (!isRemoteProxySession() || !path.startsWith(LOCAL_ADMIN_PREFIX)) {
    return path
  }
  const rest = path.slice(LOCAL_ADMIN_PREFIX.length)
  // 成段匹配：后面要么到头，要么是 / ? #，否则是同前缀的其它路径。
  if (rest !== '' && !/^[/?#]/.test(rest)) {
    return path
  }
  return REMOTE_ADMIN_PREFIX + rest
}

function normalizePath(path: string): string {
  return path.startsWith('/') ? path : `/${path}`
}

function normalizeAPIBaseURL(value: unknown): string {
  const raw = String(value || DEFAULT_API_BASE_URL).trim() || DEFAULT_API_BASE_URL
  const withoutTrailingSlash = raw.replace(/\/+$/, '')
  if (/^[a-z][a-z\d+.-]*:\/\//i.test(withoutTrailingSlash) || withoutTrailingSlash.startsWith('//')) {
    return withoutTrailingSlash
  }
  return normalizePath(withoutTrailingSlash)
}

export function getAPIBaseURL(): string {
  return API_BASE_URL
}

export function buildApiUrl(path: string): string {
  const base = getAPIBaseURL().replace(/\/+$/, '')
  // 绕过 axios 拦截器的调用点（SSE、原生 fetch）也要走远程改写。
  let suffix = rewriteAdminPathForRemote(normalizePath(path))
  if (suffix === DEFAULT_API_BASE_URL) {
    suffix = ''
  } else if (suffix.startsWith(`${DEFAULT_API_BASE_URL}/`)) {
    suffix = suffix.slice(DEFAULT_API_BASE_URL.length)
  }
  return `${base}${suffix}`
}

export function buildGatewayUrl(path: string): string {
  const suffix = normalizePath(path)
  try {
    const origin =
      typeof window === 'undefined'
        ? new URL(getAPIBaseURL()).origin
        : new URL(getAPIBaseURL(), window.location.origin).origin
    return `${origin}${suffix}`
  } catch {
    return suffix
  }
}
