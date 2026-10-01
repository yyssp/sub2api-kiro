package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	kiropkg "github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

// kiroAvailableProfile 对应 ListAvailableProfiles API 返回的单个 profile。
type kiroAvailableProfile struct {
	ARN         string `json:"arn"`
	ProfileName string `json:"profileName"`
}

// kiroListAvailableProfilesResponse 对应 ListAvailableProfiles API 的响应。
type kiroListAvailableProfilesResponse struct {
	Profiles  []kiroAvailableProfile `json:"profiles"`
	NextToken string                 `json:"nextToken"`
}

// firstARN 返回第一个非空的真实 profileArn。
func (r *kiroListAvailableProfilesResponse) firstARN() string {
	for _, p := range r.Profiles {
		if arn := strings.TrimSpace(p.ARN); arn != "" {
			return arn
		}
	}
	return ""
}

// kiroProfileResolutionGroup 合并同一账号的并发解析，避免 N 个并发请求打 N 次 ListAvailableProfiles。
var kiroProfileResolutionGroup singleflight.Group

// kiroProfileResolutionFlight 记录每个账号下次允许发起解析的时间（map[int64]kiroProfileBackoff）。
//
// 过去用 sync.Once：首次解析失败就把占位 ARN 永久定死，进程不重启永不重试，
// 企业账号会一直带着错误的 profileArn 打上游。现在失败按指数退避重试。
var kiroProfileResolutionFlight sync.Map

const (
	kiroProfileResolveBackoffMin = 5 * time.Second
	kiroProfileResolveBackoffMax = 60 * time.Second
	// 上游确认没有企业 profile 时结果稳定，长时间内不必再查。
	kiroProfileResolveNoProfileTTL = 30 * time.Minute
)

type kiroProfileBackoff struct {
	until    time.Time
	failures int
}

func kiroProfileResolutionDeferred(accountID int64, now time.Time) bool {
	v, ok := kiroProfileResolutionFlight.Load(accountID)
	if !ok {
		return false
	}
	backoff, ok := v.(kiroProfileBackoff)
	return ok && now.Before(backoff.until)
}

func kiroRecordProfileResolutionFailure(accountID int64, now time.Time) {
	failures := 1
	if v, ok := kiroProfileResolutionFlight.Load(accountID); ok {
		if prev, ok := v.(kiroProfileBackoff); ok {
			failures = prev.failures + 1
		}
	}
	delay := kiroProfileResolveBackoffMin << min(failures-1, 4)
	if delay > kiroProfileResolveBackoffMax {
		delay = kiroProfileResolveBackoffMax
	}
	kiroProfileResolutionFlight.Store(accountID, kiroProfileBackoff{until: now.Add(delay), failures: failures})
}

// kiroListAvailableProfilesFn 是解析入口使用的调用点，测试替换它来模拟上游。
var kiroListAvailableProfilesFn = kiroListAvailableProfiles

// kiroListAvailableProfiles 调用 AWS CodeWhisperer ListAvailableProfiles API 获取真实 profileArn。
//
// API: POST https://q.{region}.amazonaws.com/
// Header: x-amz-target: AmazonCodeWhispererService.ListAvailableProfiles
// Content-Type: application/x-amz-json-1.0
// Body: {"maxResults":10}
func kiroListAvailableProfiles(ctx context.Context, account *Account, token string) (*kiroListAvailableProfilesResponse, error) {
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}
	region := kiroAPIRegion(account)
	host := fmt.Sprintf("q.%s.amazonaws.com", region)
	endpointURL := fmt.Sprintf("https://%s/", host)

	reqBody := `{"maxResults":10}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, strings.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create list profiles request: %w", err)
	}

	accountKey := buildKiroAccountKey(account)
	machineID := buildKiroMachineID(account)

	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "AmazonCodeWhispererService.ListAvailableProfiles")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", kiropkg.BuildRuntimeUserAgent(accountKey, machineID))
	req.Header.Set("X-Amz-User-Agent", kiropkg.BuildRuntimeAmzUserAgent(accountKey, machineID))
	req.Header.Set("Amz-Sdk-Request", "attempt=1; max=1")
	req.Header.Set("Amz-Sdk-Invocation-Id", uuid.NewString())
	applyKiroConditionalHeaders(req, account)

	proxyURL := kiroProxyURL(account)
	client, err := httpclient.GetClient(httpclient.Options{
		ProxyURL:           proxyURL,
		Timeout:            30 * time.Second,
		ValidateResolvedIP: true,
	})
	if err != nil {
		return nil, fmt.Errorf("create http client: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list available profiles request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list available profiles: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed kiroListAvailableProfilesResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &parsed, nil
}

// kiroResolveAndPersistProfileArn 解析并回填 Enterprise/IdC 账号的真实 profileArn（包级共用函数）。
//
// 流式端点（generateAssistantResponse）强制要求 profileArn：不带 → 400
// "profileArn is required for this request"。Enterprise/IdC 账号的 OAuth 流程
// 通常不返回 profileArn，凭据中可能为空或 BuilderID 占位符，需要通过
// ListAvailableProfiles API 获取真实 ARN。
//
// 行为：
//   - API Key 凭据 / 已有真实（非占位符）profileArn → 直接返回，不发起网络请求
//   - BuilderId / Social 账号（确定无企业 profile）→ 直接回填默认 ARN，跳过 ListAvailableProfiles
//     （该 API 对 Builder ID 身份必然返回 403，见 kiroAccountLacksEnterpriseProfile）
//   - 否则调用 ListAvailableProfiles，命中真实 ARN 时写回凭据并持久化到 DB
//   - 上游无 profile → 回填默认 ARN（Social → Social ARN，其余 → BuilderID 占位符）并持久化
//   - 并发请求合并为一次 API 调用；失败按 5s-60s 指数退避重试，期间用默认 ARN
func kiroResolveAndPersistProfileArn(ctx context.Context, repo AccountRepository, account *Account, token string) string {
	if account == nil {
		return ""
	}

	// API Key 凭据没有 profileArn 概念
	authMethod := strings.TrimSpace(account.GetCredential("auth_method"))
	if strings.EqualFold(authMethod, "api_key") || strings.EqualFold(authMethod, "apikey") {
		return ""
	}
	if firstKiroCredential(account, "kiro_api_key", "kiroApiKey", "api_key") != "" {
		return ""
	}

	// 已有真实 ARN（非占位符）→ 直接用
	existingARN := strings.TrimSpace(account.GetCredential("profile_arn"))
	if existingARN != "" && !kiroIsPlaceholderProfileARN(existingARN) {
		return existingARN
	}

	accountID := account.ID
	defaultARN := kiroDefaultProfileARN(account)

	if kiroAccountLacksEnterpriseProfile(account) {
		// BuilderId / Social 身份没有企业 profile，ListAvailableProfiles 必然 403，
		// 直接使用默认 ARN，省掉一次注定失败的请求。已经是默认值时不重复落库。
		if existingARN != defaultARN {
			kiroApplyProfileArn(ctx, repo, account, defaultARN, true)
		}
		return defaultARN
	}

	// 退避窗口内不再打上游，当前请求先用默认 ARN。
	if kiroProfileResolutionDeferred(accountID, time.Now()) {
		if existingARN != "" {
			return existingARN
		}
		return defaultARN
	}

	result, _, _ := kiroProfileResolutionGroup.Do(strconv.FormatInt(accountID, 10), func() (any, error) {
		profiles, err := kiroListAvailableProfilesFn(ctx, account, token)
		switch {
		case err != nil:
			// 失败只回填内存不落库：库里保留原值，退避结束后重新解析。
			kiroRecordProfileResolutionFailure(accountID, time.Now())
			logger.L().Warn("kiro profileArn resolution failed, using default until retry",
				zap.Int64("account_id", accountID),
				zap.String("profile_arn", defaultARN),
				zap.Error(err),
			)
			kiroApplyProfileArn(ctx, repo, account, defaultARN, false)
			return defaultARN, nil
		case profiles.firstARN() != "":
			kiroProfileResolutionFlight.Delete(accountID)
			arn := profiles.firstARN()
			kiroApplyProfileArn(ctx, repo, account, arn, true)
			return arn, nil
		default:
			kiroProfileResolutionFlight.Store(accountID, kiroProfileBackoff{until: time.Now().Add(kiroProfileResolveNoProfileTTL)})
			logger.L().Debug("kiro profileArn resolution: no enterprise profile found, using default",
				zap.Int64("account_id", accountID),
				zap.String("profile_arn", defaultARN),
			)
			kiroApplyProfileArn(ctx, repo, account, defaultARN, true)
			return defaultARN, nil
		}
	})
	if arn, ok := result.(string); ok && arn != "" {
		return arn
	}
	return existingARN
}

// kiroApplyProfileArn 回填内存凭据，persist 为真时同步落库。
func kiroApplyProfileArn(ctx context.Context, repo AccountRepository, account *Account, arn string, persist bool) {
	if account.Credentials == nil {
		account.Credentials = make(map[string]any)
	}
	account.Credentials["profile_arn"] = arn
	if !persist || repo == nil {
		return
	}
	if err := persistAccountCredentials(ctx, repo, account, account.Credentials); err != nil {
		logger.L().Warn("kiro profileArn persist failed (does not affect current request)",
			zap.Int64("account_id", account.ID),
			zap.Error(err),
		)
		return
	}
	logger.L().Info("kiro profileArn resolved and persisted",
		zap.Int64("account_id", account.ID),
		zap.String("profile_arn", arn),
	)
}

// resolveAndPersistKiroProfileArn 是 GatewayService 对 kiroResolveAndPersistProfileArn 的 thin wrapper。
func (s *GatewayService) resolveAndPersistKiroProfileArn(ctx context.Context, account *Account, token string) string {
	return kiroResolveAndPersistProfileArn(ctx, s.accountRepo, account, token)
}

// ensureKiroProfileArnForRequest 确保 Kiro 请求的 profileArn 已解析。
// 在流式/非流式请求发送前调用，如果是 KRS 模式且 profileArn 缺失或为占位符，
// 则触发 ListAvailableProfiles 解析并回填。
func (s *GatewayService) ensureKiroProfileArnForRequest(ctx context.Context, account *Account, token string, mode string) {
	if account == nil || mode != KiroEndpointModeKRS {
		return
	}
	existingARN := strings.TrimSpace(account.GetCredential("profile_arn"))
	if existingARN != "" && !kiroIsPlaceholderProfileARN(existingARN) {
		return
	}
	// 触发解析（内部有去重逻辑）
	_ = s.resolveAndPersistKiroProfileArn(ctx, account, token)
}
