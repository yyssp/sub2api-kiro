/**
 * 远程代管模式（Remote Proxy）
 *
 * 本部署的管理面接管另一套 sub2api 的数据：浏览器只访问本站同源地址，
 * 由本端服务端携带 Admin API Key 转发到目标部署，因此目标端可以是未经改造的
 * 原版部署，且全程不涉及浏览器跨域。
 *
 * 是否启用完全由后端配置决定（remote_proxy.enabled），前端在登录页探测后
 * 切换登录方式，无需构建期变量。
 */

import apiClient from './client'

const REMOTE_MODE_KEY = 'remote_proxy_mode'

export interface RemoteInfo {
  enabled: boolean
}

export interface RemoteLoginResponse {
  access_token: string
}

/**
 * 探测本部署是否处于远程代管模式。
 * 未启用时后端不注册该路由，404 即表示普通部署。
 */
export async function fetchRemoteInfo(): Promise<RemoteInfo | null> {
  try {
    const { data } = await apiClient.get<RemoteInfo>('/remote/info')
    return data?.enabled ? data : null
  } catch {
    return null
  }
}

/**
 * 以 Admin API Key 换取远程会话令牌。
 * Key 仅在此处经浏览器传输一次，之后保存在服务端，不落到浏览器存储。
 */
export async function loginWithAdminKey(adminKey: string): Promise<RemoteLoginResponse> {
  const { data } = await apiClient.post<RemoteLoginResponse>('/remote/login', {
    admin_key: adminKey
  })
  return data
}

export async function logoutRemote(): Promise<void> {
  try {
    await apiClient.post('/remote/logout')
  } catch {
    // 服务端撤销失败不应阻止本地登出
  }
}

// ==================== 本地会话标记 ====================

export function isRemoteSession(): boolean {
  try {
    return localStorage.getItem(REMOTE_MODE_KEY) === '1'
  } catch {
    return false
  }
}

export function markRemoteSession(): void {
  try {
    localStorage.setItem(REMOTE_MODE_KEY, '1')
  } catch {
    // ignore localStorage failures
  }
}

export function clearRemoteSession(): void {
  try {
    localStorage.removeItem(REMOTE_MODE_KEY)
  } catch {
    // ignore localStorage failures
  }
}

/**
 * 远程会话的本地身份。
 *
 * 远程模式下 /auth/me 不可用：会话不对应本部署的任何账号，而目标端的该接口
 * 仅接受 JWT、不接受 Admin API Key。管理面所需的身份信息在此本地合成。
 */
export function buildRemoteUser(): Record<string, unknown> {
  return {
    id: 0,
    username: 'admin',
    email: '',
    role: 'admin',
    status: 'active',
    balance: 0,
    created_at: new Date().toISOString()
  }
}
