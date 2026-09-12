package service

// Cursor 的 Wire 装配全部收在本文件，不动既有 Provide* 函数签名。
//
// ⚠️ 刻意不给 NewGatewayService / ProvideAccountTestService /
// ProvideAccountUsageService 加参数：那些签名是上游(nianzs)的高频修改点，
// 加一个参数就等于在每次合并时制造一处冲突。改用
// 「marker 类型 + setter」的装配方式，新增代码全部落在新文件里。

// CursorWiring 是一个只为触发 Wire 装配而存在的 marker 类型。
// Wire 会为了构造它而先构造依赖，并在构造过程中执行 setter 注入。
type CursorWiring struct{}

// ProvideCursorTokenProvider 构造 Cursor 的 token provider。
//
// 复用 GeminiTokenCache（CursorTokenCache 是它的别名），不另立缓存抽象。
func ProvideCursorTokenProvider(
	accountRepo AccountRepository,
	tokenCache GeminiTokenCache,
) *CursorTokenProvider {
	return NewCursorTokenProvider(accountRepo, tokenCache)
}

func ProvideCursorOAuthService(accountRepo AccountRepository) *CursorOAuthService {
	return NewCursorOAuthService(accountRepo)
}

// ProvideCursorWiring 把 CursorTokenProvider 注入到所有需要它的既有服务里。
//
// 返回 CursorWiring 而非 error/struct{}，是为了让 Wire 把它当成一个真实节点；
// 上层只要在依赖图里要求 CursorWiring，这段注入就一定会被执行。
func ProvideCursorWiring(
	provider *CursorTokenProvider,
	gatewayService *GatewayService,
	accountTestService *AccountTestService,
	accountUsageService *AccountUsageService,
) CursorWiring {
	if gatewayService != nil {
		gatewayService.cursorTokenProvider = provider
	}
	if accountTestService != nil {
		accountTestService.SetCursorTokenProvider(provider)
	}
	if accountUsageService != nil {
		accountUsageService.SetCursorTokenProvider(provider)
	}
	return CursorWiring{}
}
