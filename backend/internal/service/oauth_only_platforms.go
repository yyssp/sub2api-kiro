package service

func isOAuthOnlyRestrictedPlatform(platform string) bool {
	switch platform {
	case PlatformOpenAI, PlatformAntigravity, PlatformAnthropic, PlatformGemini, PlatformKiro, PlatformGrok,
		// cursor 的凭证本质是 OAuth 会话 token（session/refresh），
		// APIKey 类型仅用于用户自建网关，不走内建 Cursor 直连链路。
		PlatformCursor:
		return true
	default:
		return false
	}
}
