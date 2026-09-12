package cursor

// ⚠️ 本文件原有的 3 个测试全部依赖 ai2api 的 handler.go（未移植）：
//   - TestServeOpenAIAndResponsesSurfaceClassifiedRetryFailure
//   - TestCursorRetryFailureSurfaceClassifiesTerminalFailures
//   - TestCursorRetryFailureObserveKeepsOnlyFailedAttempts
//
// 它们覆盖的是「错误分类 → 对外 HTTP 响应」的映射，在 sub2api 侧属于
// service 层职责（cursor_error_classifier.go），且要映射到 sub2api 自己的
// 错误响应体，不能照搬 ai2api 的 body 断言。
//
// 阶段 4 实现 cursor_error_classifier.go 时必须复现以下语义（来自原测试断言）：
//   badModel=true               → 400 invalid_request_error（"model not available"，换号无用，直接返回客户端）
//   namedModelsUnavailable=true → 400 invalid_request_error（提示改用 Auto/default）
//   kind=ErrQuota               → 429 rate_limit_error（额度耗尽）
//   kind=ErrAuth                → 503 api_error（认证失败）
//   kind=ErrTransient           → 503 api_error（多次换号后仍不可用）
// 以及：只有「失败的尝试」才能覆盖已记录的失败信息，成功的尝试不得覆盖。
