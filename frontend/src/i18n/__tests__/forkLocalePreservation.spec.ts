import { createHash } from 'node:crypto'

import { describe, expect, it } from 'vitest'

import en from '@/i18n/locales/en'
import zh from '@/i18n/locales/zh'

// 本清单原本还包含 admin.accounts.oauth.kiro.importProviderLabel /
// providerMismatch,对应“先选账号来源 radio、再校验 Token JSON 里的 provider
// 是否一致”的导入界面。本仓库的 Kiro 导入已改为批量:importToken 返回
// KiroImportEntry[],支持粘贴凭据数组、按 authMethod 自动区分 oauth / apikey、
// 并按 clientIdHash 自行判断是否需要 Device Registration。单选 radio 无法描述
// 数组里 N 条不同来源的凭据,api_key 条目更是完全没有 provider 字段,所以这两个
// 键在新设计下已无调用方,从清单中移除而不是恢复旧界面(恢复会退回单对象导入)。
// 同理,deviceRegistrationRequired / tokenJsonHint 的下方哈希取的是本仓库的文案:
// 上游文案说“BuilderId / Enterprise 才需要 Device Registration”,在新逻辑下是错的。
const preservedForkKeys = `
admin.accounts.baseUrlOptional
admin.accounts.bulkRefreshTokenConfirm
admin.accounts.bulkRefreshTokenTitle
admin.accounts.bulkResetStatusConfirm
admin.accounts.bulkResetStatusTitle
admin.accounts.kiro.apiKeyHint
admin.accounts.kiro.relayApiKeyHint
admin.accounts.kiro.relayBaseUrlHint
admin.accounts.kiro.relayPriorityHint
admin.accounts.kiroAccount
admin.accounts.kiroCooldown
admin.accounts.kiroCreditUnitPriceUsd
admin.accounts.kiroCreditUnitPriceUsdHint
admin.accounts.kiroProfileError
admin.accounts.kiroProfileHint
admin.accounts.kiroRuntimeResetsAt
admin.accounts.kiroSuspended
admin.accounts.kiroUsageForbidden
admin.accounts.kiroUsageForbiddenHint
admin.accounts.oauth.kiro.authCode
admin.accounts.oauth.kiro.authCodeDesc
admin.accounts.oauth.kiro.authCodeHint
admin.accounts.oauth.kiro.authCodePlaceholder
admin.accounts.oauth.kiro.authModeTitle
admin.accounts.oauth.kiro.deviceRegistrationHint
admin.accounts.oauth.kiro.deviceRegistrationLabel
admin.accounts.oauth.kiro.deviceRegistrationRequired
admin.accounts.oauth.kiro.extIdpAuthCodeDescIdp
admin.accounts.oauth.kiro.extIdpAuthCodeDescPortal
admin.accounts.oauth.kiro.extIdpAuthCodeHintIdp
admin.accounts.oauth.kiro.extIdpAuthCodeHintPortal
admin.accounts.oauth.kiro.extIdpAuthCodePlaceholderIdp
admin.accounts.oauth.kiro.extIdpAuthCodePlaceholderPortal
admin.accounts.oauth.kiro.extIdpNewUrlBadge
admin.accounts.oauth.kiro.extIdpNextStep
admin.accounts.oauth.kiro.extIdpOpenDescIdp
admin.accounts.oauth.kiro.extIdpOpenDescPortal
admin.accounts.oauth.kiro.extIdpStageHint
admin.accounts.oauth.kiro.extIdpStageIdp
admin.accounts.oauth.kiro.extIdpStagePortal
admin.accounts.oauth.kiro.extIdpStep2Idp
admin.accounts.oauth.kiro.extIdpStep2Portal
admin.accounts.oauth.kiro.extIdpStep3Idp
admin.accounts.oauth.kiro.extIdpStep3Portal
admin.accounts.oauth.kiro.externalIdpSubtitle
admin.accounts.oauth.kiro.externalIdpTitle
admin.accounts.oauth.kiro.followSteps
admin.accounts.oauth.kiro.generateAuthUrl
admin.accounts.oauth.kiro.githubDesc
admin.accounts.oauth.kiro.githubOauth
admin.accounts.oauth.kiro.githubTitle
admin.accounts.oauth.kiro.googleDesc
admin.accounts.oauth.kiro.googleOauth
admin.accounts.oauth.kiro.googleTitle
admin.accounts.oauth.kiro.idcLogin
admin.accounts.oauth.kiro.idcStartUrlLabel
admin.accounts.oauth.kiro.idcSubtitle
admin.accounts.oauth.kiro.idcTitle
admin.accounts.oauth.kiro.importAndUpdate
admin.accounts.oauth.kiro.importDialogTitle
admin.accounts.oauth.kiro.importSubtitle
admin.accounts.oauth.kiro.importTitle
admin.accounts.oauth.kiro.importTokenFile
admin.accounts.oauth.kiro.oauthProviderTitle
admin.accounts.oauth.kiro.oauthSubtitle
admin.accounts.oauth.kiro.oauthTitle
admin.accounts.oauth.kiro.openUrlDesc
admin.accounts.oauth.kiro.regionLabel
admin.accounts.oauth.kiro.regionPlaceholder
admin.accounts.oauth.kiro.socialSubtitle
admin.accounts.oauth.kiro.startUrlLabel
admin.accounts.oauth.kiro.startUrlPlaceholder
admin.accounts.oauth.kiro.step1GenerateUrl
admin.accounts.oauth.kiro.step2OpenUrl
admin.accounts.oauth.kiro.step3EnterCode
admin.accounts.oauth.kiro.title
admin.accounts.oauth.kiro.tokenJsonHint
admin.accounts.oauth.kiro.tokenJsonInvalid
admin.accounts.oauth.kiro.tokenJsonLabel
admin.accounts.oauth.kiro.tokenJsonRequired
admin.accounts.platforms.kiro
admin.accounts.stats.approxCost
admin.accounts.stats.kiroCredits
admin.accounts.types.kiroApikey
admin.accounts.types.kiroApikeyRelay
admin.accounts.types.kiroOauth
admin.accounts.usageWindow.kiroBonus
admin.accounts.usageWindow.kiroCredits
admin.accounts.usageWindow.kiroDaysLeft
admin.accounts.usageWindow.kiroExpires
admin.accounts.usageWindow.kiroReset
admin.channels.form.fillDefaultModels
admin.channels.form.fillDefaultModelsAlreadyConfigured
admin.channels.form.fillDefaultModelsSuccess
admin.channels.form.fillingDefaultModels
admin.groups.kiroCache.description
admin.groups.kiroCache.enabled
admin.groups.kiroCache.endpointMode
admin.groups.kiroCache.endpointModeAuto
admin.groups.kiroCache.endpointModeHint
admin.groups.kiroCache.endpointModeKRS
admin.groups.kiroCache.endpointModeQ
admin.groups.kiroCache.ratio
admin.groups.kiroCache.ratioHint
admin.groups.kiroCache.stickyRouting
admin.groups.kiroCache.stickyRoutingHint
admin.groups.kiroCache.stickyTTL
admin.groups.kiroCache.stickyTTLHint
admin.groups.kiroCache.title
admin.groups.platforms.kiro
admin.usage.cleanup.errorConfirm
admin.usage.cleanup.errorSubmitFailed
admin.usage.cleanup.errorSubmitSuccess
admin.users.columns.usageGrok
admin.users.columns.usageKiro
home.providers.grok
home.providers.kiro
`.trim().split(/\s+/)

// kiro 的 importTitle / importSubtitle / importDialogTitle 下方哈希取的是
// 批量导入并入「添加账号」弹层之后的文案：独立导入入口已删除，导入不再局限于
// Kiro IDE 单 token，而是 JSON / ksk / Token / Kiro IDE 四种凭证形态，
// 原文案「从 Kiro IDE 导入 / 已在 Kiro IDE 登录时使用」在新逻辑下是错的。
const expectedHashes = {
  en: '7ec1a34cd7f2379c7f1b4771c14c253db5abe4a3c3366dc6a56ab072714ab915',
  zh: 'dc7a0d9e431b2e58b55b91927b1e0a5cd16a6db8547d1a10939a44eac7cb028b',
}

function localeValue(locale: Record<string, unknown>, key: string): unknown {
  return key.split('.').reduce<unknown>((value, segment) => {
    if (!value || typeof value !== 'object') return undefined
    return (value as Record<string, unknown>)[segment]
  }, locale)
}

function preservedValuesHash(locale: Record<string, unknown>): string {
  const payload = [...preservedForkKeys]
    .sort()
    .map((key) => `${key}\0${String(localeValue(locale, key))}`)
    .join('\n')
  return createHash('sha256').update(payload).digest('hex')
}

describe.each([
  ['en', en, expectedHashes.en],
  ['zh', zh, expectedHashes.zh],
] as const)('fork locale preservation: %s', (_name, locale, expectedHash) => {
  it('keeps every fork-added key', () => {
    expect(preservedForkKeys).toHaveLength(117)
    expect(preservedForkKeys.filter((key) => localeValue(locale, key) === undefined)).toEqual([])
  })

  it('keeps the exact fork translations', () => {
    expect(preservedValuesHash(locale)).toBe(expectedHash)
  })
})
