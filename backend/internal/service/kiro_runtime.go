package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	kiropkg "github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/cespare/xxhash/v2"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

type kiroEndpointConfig struct {
	URL       string
	AmzTarget string
	Name      string
}

const kiroInvalidModelTempUnschedDuration = time.Minute

const (
	kiroRetryBaseDelay = 200 * time.Millisecond
	kiroRetryMaxDelay  = 2 * time.Second
)

var kiroRetrySleep = sleepWithContext

func kiroRetryBackoffDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := kiroRetryBaseDelay * time.Duration(1<<attempt)
	if delay > kiroRetryMaxDelay {
		delay = kiroRetryMaxDelay
	}
	jitterMax := delay / 4
	if jitterMax <= 0 {
		return delay
	}
	return delay + time.Duration(mathrand.Int63n(int64(jitterMax)+1))
}

func sleepKiroRetry(ctx context.Context, attempt int) error {
	return kiroRetrySleep(ctx, kiroRetryBackoffDelay(attempt))
}

func resolveKiroUpstreamModel(mappedModel string) string {
	upstreamModel := kiropkg.MapModel(mappedModel)
	if strings.TrimSpace(upstreamModel) == "" {
		upstreamModel = mappedModel
	}
	return upstreamModel
}

func (s *GatewayService) forwardKiroMessages(ctx context.Context, c *gin.Context, account *Account, parsed *ParsedRequest, startTime time.Time) (*ForwardResult, error) {
	if account == nil || parsed == nil {
		return nil, fmt.Errorf("kiro forward: missing account or request")
	}

	originalModel := parsed.Model
	mappedModel := originalModel
	if next := account.GetMappedModel(originalModel); next != "" {
		mappedModel = next
	}
	body := parsed.Body.Bytes()
	if mappedModel != originalModel {
		body = s.replaceModelInBody(body, mappedModel)
	}
	logger.L().Debug("gateway forward_kiro_messages: request prepared",
		zap.Int64("account_id", account.ID),
		zap.String("auth_method", strings.TrimSpace(account.GetCredential("auth_method"))),
		zap.String("requested_model", originalModel),
		zap.String("mapped_model", mappedModel),
		zap.Bool("has_profile_arn", strings.TrimSpace(account.GetCredential("profile_arn")) != ""),
	)

	if s.shouldEmulateWebSearch(ctx, account, parsed.GroupID, body) {
		parsedForEmulation, err := parsed.CloneForBody(body)
		if err != nil {
			return nil, err
		}
		parsedForEmulation.Model = mappedModel
		return s.handleWebSearchEmulation(ctx, c, account, parsedForEmulation)
	}

	if parsed.Stream {
		inputTokens := estimateKiroInputTokens(ctx, body)
		cachePlan := prepareCachePlanForContext(
			ctx, c, account, parsed.Group, body, mappedModel,
			"anthropic_messages", inputTokens,
		)
		resp, _, err := s.openKiroAnthropicStreamResponse(
			ctx, account, parsed, body, mappedModel, originalModel,
			c.Request.Header, parsed.Group, cachePlan,
		)
		if err != nil {
			var failoverErr *UpstreamFailoverError
			if errors.As(err, &failoverErr) {
				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					Platform:           account.Platform,
					AccountID:          account.ID,
					AccountName:        account.Name,
					UpstreamStatusCode: failoverErr.StatusCode,
					Kind:               "failover",
					Message:            sanitizeUpstreamErrorMessage(err.Error()),
				})
				return nil, failoverErr
			}
			// 必须在通用 502 之前判：behavior=reject 时请求根本没发出去，
			// 报 "Upstream request failed" 会把用户引去排查上游。
			// 这条分支是流式路径 —— Claude Code CLI 等客户端只走流式，
			// 只在非流式分支加映射等于没加（2026-09-14 交互式实测发现）。
			if respondKiroPayloadTooLarge(c, err) {
				return nil, err
			}
			safeErr := sanitizeUpstreamErrorMessage(err.Error())
			setOpsUpstreamError(c, 0, safeErr, "")
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: 0,
				Kind:               "request_error",
				Message:            safeErr,
			})
			c.JSON(http.StatusBadGateway, gin.H{
				"type": "error",
				"error": gin.H{
					"type":    "api_error",
					"message": "Upstream request failed",
				},
			})
			return nil, fmt.Errorf("kiro upstream request failed: %s", safeErr)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode >= 400 {
			return nil, s.handleKiroHTTPError(ctx, resp, c, account, mappedModel, body)
		}
		upstreamModel := resolveKiroUpstreamModel(mappedModel)
		// 守卫通知头必须直接写到 gin 的 ResponseWriter 上。
		// openKiroAnthropicStreamResponse 把它们塞在 resp.Header 里，而
		// handleStreamingResponse 是按**上游头白名单**(responseheaders.FilterHeaders)
		// 转发的 —— 这几个是我们自己加的头，不在白名单里，会被整组丢掉。
		// 结果就是裁了半部历史仍返回 200 且响应上毫无痕迹(2026-09-14 实测:
		// dropped_history_items=14，客户端一个 x-sub2api-context-* 都收不到)。
		copyKiroTrimHeaders(c.Writer.Header(), resp.Header)
		streamResult, err := s.handleStreamingResponse(ctx, resp, c, account, startTime, originalModel, mappedModel, false)
		if err != nil {
			return nil, err
		}
		if streamResult.usage == nil {
			streamResult.usage = &ClaudeUsage{}
		}
		requestID := buildKiroRequestID(resp)
		return &ForwardResult{
			RequestID:        requestID,
			Usage:            *streamResult.usage,
			Model:            originalModel,
			UpstreamModel:    upstreamModel,
			Stream:           true,
			Duration:         time.Since(startTime),
			FirstTokenMs:     streamResult.firstTokenMs,
			ClientDisconnect: streamResult.clientDisconnect,
		}, nil
	}

	token, tokenType, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	if tokenType != "oauth" && tokenType != "apikey" {
		return nil, fmt.Errorf("kiro requires oauth or apikey token, got %s", tokenType)
	}
	if isOnlyWebSearchToolInBody(body) {
		webSearchResult, webSearchErr := s.executeKiroWebSearch(ctx, account, parsed.Group, body, mappedModel, originalModel, token, c.Request.Header)
		switch {
		case errors.Is(webSearchErr, errKiroWebSearchFallback):
		case webSearchErr == nil:
			upstreamModel := resolveKiroUpstreamModel(mappedModel)
			c.Header("Content-Type", "application/json")
			claudeReqID := kiropkg.NewClaudeRequestID()
			c.Header("x-request-id", claudeReqID)
			c.Header("request-id", claudeReqID)
			c.Data(http.StatusOK, "application/json", webSearchResult.ResponseBody)
			return &ForwardResult{
				RequestID:     webSearchResult.RequestID,
				Usage:         webSearchResult.Usage,
				Model:         originalModel,
				UpstreamModel: upstreamModel,
				Stream:        false,
				Duration:      time.Since(startTime),
			}, nil
		default:
			var httpErr *kiroWebSearchHTTPError
			if errors.As(webSearchErr, &httpErr) && httpErr.Response != nil {
				return nil, s.handleKiroHTTPError(ctx, httpErr.Response, c, account, mappedModel, body)
			}
			var failoverErr *UpstreamFailoverError
			if errors.As(webSearchErr, &failoverErr) {
				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					Platform:           account.Platform,
					AccountID:          account.ID,
					AccountName:        account.Name,
					UpstreamStatusCode: failoverErr.StatusCode,
					Kind:               "failover",
					Message:            sanitizeUpstreamErrorMessage(webSearchErr.Error()),
				})
				return nil, failoverErr
			}
			safeErr := sanitizeUpstreamErrorMessage(webSearchErr.Error())
			c.JSON(http.StatusBadGateway, gin.H{
				"type": "error",
				"error": gin.H{
					"type":    "api_error",
					"message": "Upstream request failed",
				},
			})
			return nil, fmt.Errorf("kiro upstream request failed: %s", safeErr)
		}
	}

	inputTokens := estimateKiroInputTokens(ctx, body)
	prepareCachePlanForContext(
		ctx, c, account, parsed.Group, body, mappedModel,
		"anthropic_messages", inputTokens,
	)
	resp, requestCtx, err := s.executeKiroUpstreamWithParsed(ctx, account, parsed, body, mappedModel, originalModel, token, c.Request.Header)
	if err != nil {
		var failoverErr *UpstreamFailoverError
		if errors.As(err, &failoverErr) {
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: failoverErr.StatusCode,
				Kind:               "failover",
				Message:            sanitizeUpstreamErrorMessage(err.Error()),
			})
			return nil, failoverErr
		}
		if respondKiroPayloadTooLarge(c, err) {
			return nil, err
		}
		safeErr := sanitizeUpstreamErrorMessage(err.Error())
		c.JSON(http.StatusBadGateway, gin.H{
			"type": "error",
			"error": gin.H{
				"type":    "api_error",
				"message": "Upstream request failed",
			},
		})
		return nil, fmt.Errorf("kiro upstream request failed: %s", safeErr)
	}
	defer func() { _ = resp.Body.Close() }()
	// 在写响应体之前打头：一旦 c.JSON 写出去，头就改不动了。
	setKiroPayloadTrimHeaders(c.Writer.Header(), requestCtx)
	if resp.StatusCode >= 400 {
		return nil, s.handleKiroHTTPError(ctx, resp, c, account, mappedModel, body)
	}

	requestCtx.EstimatedInputTokens = inputTokens
	parseResult, err := kiropkg.ParseNonStreamingEventStreamWithContext(resp.Body, originalModel, requestCtx)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"type": "error",
			"error": gin.H{
				"type":    "api_error",
				"message": "Failed to parse Kiro upstream response",
			},
		})
		return nil, err
	}

	usage := kiroUsageToClaude(parseResult.Usage, inputTokens)
	// Apply the same group-bound usage projection used by the other
	// Claude-Code-compatible protocol adapters. The Kiro translator only
	// understands upstream/native usage and response shaping; policy projection
	// belongs to the protocol-neutral runtime.
	mergeAndCommitCachePlan(c, &usage, true)
	parseResult.ResponseBody = rewriteKiroClaudeResponseUsage(parseResult.ResponseBody, usage)

	c.Header("Content-Type", "application/json")
	requestID := buildKiroRequestID(resp)
	claudeReqID := kiropkg.NewClaudeRequestID()
	c.Header("x-request-id", claudeReqID)
	c.Header("request-id", claudeReqID)
	c.Data(http.StatusOK, "application/json", parseResult.ResponseBody)

	upstreamModel := resolveKiroUpstreamModel(mappedModel)

	return &ForwardResult{
		RequestID:     requestID,
		Usage:         usage,
		Model:         originalModel,
		UpstreamModel: upstreamModel,
		Stream:        false,
		Duration:      time.Since(startTime),
	}, nil
}

// rewriteKiroClaudeResponseUsage keeps the JSON response body in lockstep with
// the usage value recorded by the gateway after cache-policy projection.
// Kiro's translator builds the body before the protocol-neutral runtime sees
// the final policy, so the fields must be patched once more here.
func rewriteKiroClaudeResponseUsage(body []byte, usage ClaudeUsage) []byte {
	if len(body) == 0 {
		return body
	}
	setInt := func(path string, value int) {
		if next, err := sjson.SetBytes(body, path, value); err == nil {
			body = next
		}
	}
	deletePath := func(path string) {
		if next, err := sjson.DeleteBytes(body, path); err == nil {
			body = next
		}
	}
	setInt("usage.input_tokens", max(usage.InputTokens, 0))
	setInt("usage.output_tokens", max(usage.OutputTokens, 0))
	setInt("usage.cache_read_input_tokens", max(usage.CacheReadInputTokens, 0))
	if usage.CacheCreationInputTokens > 0 {
		setInt("usage.cache_creation_input_tokens", usage.CacheCreationInputTokens)
		setInt("usage.cache_creation.ephemeral_5m_input_tokens", max(usage.CacheCreation5mTokens, 0))
		setInt("usage.cache_creation.ephemeral_1h_input_tokens", max(usage.CacheCreation1hTokens, 0))
	} else {
		deletePath("usage.cache_creation_input_tokens")
		deletePath("usage.cache_creation")
	}
	return body
}

func (s *GatewayService) openKiroAnthropicStreamResponse(ctx context.Context, account *Account, parsed *ParsedRequest, anthropicBody []byte, mappedModel, requestModel string, headers http.Header, group *Group, cachePlanOverride *cacheEmulationPlan) (*http.Response, int, error) {
	token, tokenType, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, 0, err
	}
	// Kiro 直连 AWS 支持两类 token:OAuth access_token 与 API Key(ksk_*)。
	// API Key 模式下 GetAccessToken 返回 tokenType "apikey"(无需刷新)。
	if tokenType != "oauth" && tokenType != "apikey" {
		return nil, 0, fmt.Errorf("kiro requires oauth or apikey token, got %s", tokenType)
	}

	// 客户端断开不应取消仍在收集计费用量的上游读取；detachStreamUpstreamContext
	// 让上游请求/流式读取脱离客户端请求 ctx 的取消传播，与其它 gateway 转发路径一致。
	upstreamCtx, releaseUpstreamCtx := detachStreamUpstreamContext(ctx, true)
	defer releaseUpstreamCtx()

	inputTokens := estimateKiroInputTokens(ctx, anthropicBody)
	if isOnlyWebSearchToolInBody(anthropicBody) {
		plan := cachePlanOverride
		if plan == nil {
			plan = s.prepareCacheEmulationUsage(ctx, account, group, anthropicBody, mappedModel, inputTokens)
		}
		pr, pw := io.Pipe()
		headers := make(http.Header)
		headers.Set("Content-Type", "text/event-stream")
		go func() {
			streamErr := s.streamKiroWebSearchAsAnthropic(upstreamCtx, account, anthropicBody, mappedModel, requestModel, token, inputTokens, headers, pw, plan)
			if streamErr != nil {
				_ = pw.CloseWithError(streamErr)
				return
			}
			_ = pw.Close()
		}()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     headers,
			Body:       pr,
		}, inputTokens, nil
	}

	resp, requestCtx, err := s.executeKiroUpstreamWithParsed(upstreamCtx, account, parsed, anthropicBody, mappedModel, requestModel, token, headers)
	if err != nil {
		var failoverErr *UpstreamFailoverError
		if errors.As(err, &failoverErr) {
			return nil, inputTokens, err
		}
		return nil, inputTokens, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, inputTokens, nil
	}
	plan := cachePlanOverride
	if plan == nil {
		plan = s.prepareCacheEmulationUsage(ctx, account, group, anthropicBody, mappedModel, inputTokens)
	}
	requestCtx.CacheEmulationUsage = plan.result().toKiroUsage()

	pr, pw := io.Pipe()
	wrappedHeaders := resp.Header.Clone()
	wrappedHeaders.Set("Content-Type", "text/event-stream")
	claudeReqID := kiropkg.NewClaudeRequestID()
	wrappedHeaders.Set("x-request-id", claudeReqID)
	wrappedHeaders.Set("request-id", claudeReqID)
	setKiroPayloadTrimHeaders(wrappedHeaders, requestCtx)

	go func() {
		defer func() { _ = resp.Body.Close() }()
		_, streamErr := kiropkg.StreamEventStreamAsAnthropicWithContext(upstreamCtx, resp.Body, pw, requestModel, inputTokens, requestCtx)
		if streamErr != nil {
			_, _ = io.WriteString(pw, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"stream interrupted\"}}\n\n")
			_ = pw.CloseWithError(streamErr)
			return
		}
		// Cache prefixes are persisted only after the complete upstream stream
		// has been transformed successfully. An HTTP 2xx alone is insufficient:
		// a truncated stream or client cancellation must not poison the next
		// request's cache-read accounting.
		plan.commit()
		_ = pw.Close()
	}()

	return &http.Response{
		StatusCode: resp.StatusCode,
		Header:     wrappedHeaders,
		Body:       pr,
	}, inputTokens, nil
}

func (s *GatewayService) executeKiroUpstream(ctx context.Context, account *Account, anthropicBody []byte, mappedModel, requestModel, token string, headers http.Header) (*http.Response, kiropkg.KiroRequestContext, error) {
	return s.executeKiroUpstreamWithParsed(ctx, account, nil, anthropicBody, mappedModel, requestModel, token, headers)
}

func (s *GatewayService) executeKiroUpstreamWithParsed(ctx context.Context, account *Account, parsed *ParsedRequest, anthropicBody []byte, mappedModel, requestModel, token string, headers http.Header) (*http.Response, kiropkg.KiroRequestContext, error) {
	var requestCtx kiropkg.KiroRequestContext
	mode := kiroEndpointModeForRequest(account, parsed)
	// KRS/Auto 模式：确保 profileArn 已解析（已有值时零开销，仅为安全兜底）
	if mode == KiroEndpointModeAuto || mode == KiroEndpointModeKRS {
		s.ensureKiroProfileArnForRequest(ctx, account, token, KiroEndpointModeKRS)
	}
	accountKey := buildKiroAccountKey(account)
	if err := s.checkKiroCooldown(ctx, accountKey); err != nil {
		if failoverErr := asKiroCooldownFailoverError(err); failoverErr != nil {
			return nil, requestCtx, failoverErr
		}
		return nil, requestCtx, err
	}

	modelID := kiropkg.MapModel(mappedModel)
	currentToken := token

	endpoints := buildKiroEndpoints(account, mode)
	proxyURL := kiroProxyURL(account)
	tlsProfile := s.tlsFPProfileService.ResolveTLSProfile(account)
	maxRetries := 2

	for idx, endpoint := range endpoints {
		// Q / KRS 端点现在都强制要求 profileArn：缺失时上游返回
		// 403 "User is not authorized to make this call."。按账号类型解析
		// （API Key → 空；其余 凭据真实 ARN > Social ARN > Builder ID 占位符）。
		profileArn := kiroResolveRequestProfileArn(account)
		buildResult, err := s.buildKiroPayloadForAccountWithArn(ctx, account, parsed, anthropicBody, modelID, currentToken, requestModel, headers, profileArn)
		if err != nil {
			// behavior=reject 会在这里带着 ErrKiroPayloadTooLarge 提前返回，
			// 走不到下面的 logKiroPayloadTrim —— 结果是客户端收到 413，
			// 服务端却一条 kiro.payload_rejected 都没有（2026-09-14 实测）。
			// 拒绝是最需要可观测的那条路径：没有日志，运维无从判断
			// 是阈值配置过严还是客户端真的发了超大请求。
			if weight, limit, ok := kiroPayloadTooLargeDetail(err); ok {
				fields := []zap.Field{
					zap.Int("original_weight", weight),
					zap.Int("limit_weight", limit),
				}
				if account != nil {
					fields = append(fields, zap.Int64("account_id", account.ID))
				}
				logger.L().Warn("kiro.payload_rejected", fields...)
			}
			return nil, requestCtx, err
		}
		payload := buildResult.Payload
		requestCtx = buildResult.Context
		logKiroStatelessReplay(account, buildResult.Payload)
		logKiroPayloadTrim(account, requestCtx)

		// 体积缩减重试每个端点最多一次：缩完还 400 说明问题不在体积，
		// 再试只会白烧配额。
		oversizeRetried := false

		for attempt := 0; attempt <= maxRetries; attempt++ {
			req, err := newKiroJSONRequest(ctx, endpoint.URL, payload, currentToken, accountKey, buildKiroMachineID(account), endpoint.AmzTarget, account)
			if err != nil {
				return nil, requestCtx, err
			}

			resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, tlsProfile)
			if err != nil {
				if attempt < maxRetries {
					if sleepErr := sleepKiroRetry(ctx, attempt); sleepErr != nil {
						return nil, requestCtx, sleepErr
					}
					continue
				}
				return nil, requestCtx, err
			}

			if resp.StatusCode == http.StatusTooManyRequests {
				dumpKiro429ResponseForDebug(resp, account.ID, endpoint.URL, endpoint.Name)

				cooldown, err := s.markKiro429(ctx, account.ID, accountKey)
				if err != nil {
					_ = resp.Body.Close()
					return nil, requestCtx, err
				}
				if idx+1 < len(endpoints) {
					_ = resp.Body.Close()
					if sleepErr := sleepKiroRetry(ctx, attempt); sleepErr != nil {
						return nil, requestCtx, sleepErr
					}
					break
				}
				resp.Header.Set("x-kiro-cooldown", cooldown.String())
				return resp, requestCtx, nil
			}

			if resp.StatusCode == http.StatusRequestTimeout || (resp.StatusCode >= 500 && resp.StatusCode < 600) {
				if attempt < maxRetries {
					_ = resp.Body.Close()
					if sleepErr := sleepKiroRetry(ctx, attempt); sleepErr != nil {
						return nil, requestCtx, sleepErr
					}
					continue
				}
				if idx+1 < len(endpoints) {
					_ = resp.Body.Close()
					if sleepErr := sleepKiroRetry(ctx, attempt); sleepErr != nil {
						return nil, requestCtx, sleepErr
					}
					break
				}
				return resp, requestCtx, nil
			}

			if resp.StatusCode == http.StatusPaymentRequired {
				respBody, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil {
					return nil, requestCtx, readErr
				}
				classification := classifyKiroHTTPError(resp.StatusCode, string(respBody))
				if classification.Category == kiroErrorMonthlyRequest {
					s.markKiroMonthlyRequestCountRateLimited(ctx, account, string(respBody))
				}
				return nil, requestCtx, &UpstreamFailoverError{
					StatusCode:      resp.StatusCode,
					ResponseBody:    respBody,
					ResponseHeaders: resp.Header.Clone(),
				}
			}

			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				respBody, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil {
					return nil, requestCtx, readErr
				}

				if resp.StatusCode == http.StatusForbidden && isKiroSuspendedBody(respBody) {
					if _, err := s.markKiroSuspended(ctx, accountKey); err != nil {
						return nil, requestCtx, err
					}
					resetHTTPResponseBody(resp, respBody)
					return resp, requestCtx, nil
				}

				if s.kiroTokenProvider != nil && (resp.StatusCode == http.StatusUnauthorized || isKiroTokenErrorBody(respBody)) && attempt < maxRetries {
					refreshedToken, refreshErr := s.kiroTokenProvider.ForceRefreshAccessToken(ctx, account)
					if refreshErr == nil && strings.TrimSpace(refreshedToken) != "" {
						currentToken = refreshedToken
						accountKey = buildKiroAccountKey(account)
						// 凭据可能已被 token 刷新更新，重新解析 profileArn（所有端点都需要）
						profileArn = kiroResolveRequestProfileArn(account)
						buildResult, err = s.buildKiroPayloadForAccountWithArn(ctx, account, parsed, anthropicBody, modelID, currentToken, requestModel, headers, profileArn)
						if err != nil {
							return nil, requestCtx, err
						}
						payload = buildResult.Payload
						requestCtx = buildResult.Context
						logKiroStatelessReplay(account, buildResult.Payload)
						if sleepErr := sleepKiroRetry(ctx, attempt); sleepErr != nil {
							return nil, requestCtx, sleepErr
						}
						continue
					}
					if refreshErr != nil && isNonRetryableRefreshError(refreshErr) {
						resetHTTPResponseBody(resp, respBody)
						return resp, requestCtx, nil
					}
				}

				if classifyKiroHTTPError(resp.StatusCode, string(respBody)).Category == kiroErrorAuthError {
					s.markKiroAuthTemporarilyUnavailable(ctx, account, resp.StatusCode, string(respBody))
				}

				resetHTTPResponseBody(resp, respBody)
				return resp, requestCtx, nil
			}

			if resp.StatusCode == http.StatusBadRequest {
				respBody, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil {
					return nil, requestCtx, readErr
				}
				classification := classifyKiroHTTPError(resp.StatusCode, string(respBody))
				logKiroBadRequestClassification(classification, account, mappedModel, resp.Header, respBody)

				// on_upstream_400：上游明确说体积超限时，才做压缩+裁剪并重试一次。
				// 这是"按上游真实判定缩减"而非"按我们猜的阈值预先缩减"，
				// 好处是不会对上游其实能接受的请求做无谓的有损处理。
				if classification.Category == kiroErrorBadRequestOversize && !oversizeRetried {
					oversizeRetried = true
					shrunk, buildErr := s.buildKiroPayloadWithGuard(ctx, account, parsed, anthropicBody, modelID, currentToken, requestModel, headers, profileArn,
						kiropkg.KiroPayloadGuardConfig{Behavior: kiropkg.KiroOversizeCompressThenTrim})
					if buildErr == nil && shrunk.Context.PayloadTrimStats().Triggered() {
						payload = shrunk.Payload
						requestCtx = shrunk.Context
						logKiroPayloadTrim(account, requestCtx)
						logger.L().Warn("kiro.oversize_retry_after_upstream_400",
							zap.Int64("account_id", account.ID),
							zap.Int("original_weight", requestCtx.PayloadTrimStats().OriginalWeight),
							zap.Int("final_weight", requestCtx.PayloadTrimStats().FinalWeight))
						continue
					}
					// 缩不动就别空转重试 —— 原样把 400 返回给调用方。
				}

				resetHTTPResponseBody(resp, respBody)
				return resp, requestCtx, nil
			}

			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				if err := s.markKiroSuccess(ctx, account.ID, accountKey); err != nil {
					_ = resp.Body.Close()
					return nil, requestCtx, err
				}
			}
			return resp, requestCtx, nil
		}
	}
	return nil, requestCtx, fmt.Errorf("kiro upstream endpoints exhausted")
}

// kiroKRSEndpointURL 是 Kiro 自家前置网关（KRS = Kiro Runtime Service）的固定 URL。
// KRS 仅支持 us-east-1 / eu-central-1 两个 region；这里固定走 us-east-1。
const kiroKRSEndpointURL = "https://runtime.us-east-1.kiro.dev/generateAssistantResponse"

func buildKiroEndpoints(account *Account, mode string) []kiroEndpointConfig {
	if mode == KiroEndpointModeKRS {
		return []kiroEndpointConfig{
			{
				URL:  kiroKRSEndpointURL,
				Name: "KiroRuntime",
			},
		}
	}
	region := kiroAPIRegion(account)
	qEndpoint := kiroEndpointConfig{
		URL:  fmt.Sprintf("https://q.%s.amazonaws.com/generateAssistantResponse", region),
		Name: "AmazonQ",
	}
	if mode == KiroEndpointModeAuto {
		// auto 模式：Q 优先；429、408/5xx 等可重试上游错误会切换到 KRS。
		return []kiroEndpointConfig{
			qEndpoint,
			{
				URL:  kiroKRSEndpointURL,
				Name: "KiroRuntime",
			},
		}
	}
	return []kiroEndpointConfig{qEndpoint}
}

// kiroEndpointModeForRequest 从 ParsedRequest 取 group 配置的 Kiro endpoint 模式；
// parsed/Group 为 nil 时安全兜底为 "q"。
//
// API Key 账号强制走 Q 端点(q.{region}.amazonaws.com):KRS 网关
// (runtime.us-east-1.kiro.dev)是 Kiro 自家 OAuth 网关,只认 OAuth/IdC token +
// profileArn,不接受 AWS 的 ksk_ API Key(会返回 403 "bearer token invalid")。
// 与 kiro.rs 一致——其 API Key 模式也只走 q.{region}.amazonaws.com。
func kiroEndpointModeForRequest(account *Account, parsed *ParsedRequest) string {
	if account != nil && account.Type == AccountTypeAPIKey {
		return KiroEndpointModeQ
	}
	if parsed == nil || parsed.Group == nil {
		return KiroEndpointModeQ
	}
	return parsed.Group.EffectiveKiroEndpointMode()
}

// kiroPayloadGuardConfig 把页面配置转成 kiro 包的守卫配置。
// settingService 缺失(部分单测直接构造 GatewayService)时回落到包内默认值。
func (s *GatewayService) kiroPayloadGuardConfig(ctx context.Context) kiropkg.KiroPayloadGuardConfig {
	if s == nil || s.settingService == nil {
		return kiropkg.KiroPayloadGuardConfig{}
	}
	behavior, threshold := s.settingService.GetKiroPayloadGuardSettings(ctx)
	return kiropkg.KiroPayloadGuardConfig{
		MaxWeight: threshold,
		Behavior:  toKiroGuardBehavior(behavior),
	}
}

// buildKiroPayloadForAccountWithArn 使用显式 profileArn 构建 Kiro 请求 payload。
// auto 模式下 Q/KRS 端点需要不同 profileArn，调用方按端点维度传入。
func (s *GatewayService) buildKiroPayloadForAccountWithArn(ctx context.Context, account *Account, parsed *ParsedRequest, anthropicBody []byte, modelID, token, requestModel string, headers http.Header, profileArn string) (*kiropkg.KiroBuildResult, error) {
	return s.buildKiroPayloadWithGuard(ctx, account, parsed, anthropicBody, modelID, token, requestModel, headers, profileArn, s.kiroPayloadGuardConfig(ctx))
}

func (s *GatewayService) buildKiroPayloadWithGuard(ctx context.Context, account *Account, parsed *ParsedRequest, anthropicBody []byte, modelID, token, requestModel string, headers http.Header, profileArn string, guard kiropkg.KiroPayloadGuardConfig) (*kiropkg.KiroBuildResult, error) {
	_ = ctx
	_ = token
	anthropicBody = prepareKiroPayloadBodyForRequestModel(anthropicBody, requestModel)
	buildResult, err := kiropkg.BuildKiroPayloadWithGuard(anthropicBody, modelID, profileArn, "AI_EDITOR", headers, guard)
	if err != nil {
		return nil, err
	}
	if stableID := stableKiroConversationID(account, parsed, anthropicBody, modelID, profileArn); stableID != "" {
		if next, setErr := sjson.SetBytes(buildResult.Payload, "conversationState.conversationId", stableID); setErr == nil {
			buildResult.Payload = next
		}
	}
	return buildResult, nil
}

func stableKiroConversationID(account *Account, parsed *ParsedRequest, anthropicBody []byte, modelID, profileArn string) string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SUB2API_KIRO_CONVERSATION_ID_MODE"))) {
	case "random", "uuid", "off", "false", "0":
		return ""
	}
	seed := stableKiroConversationSeed(account, parsed, anthropicBody, modelID, profileArn)
	if seed == "" {
		return ""
	}
	return generateSessionUUID(seed)
}

func stableKiroConversationSeed(account *Account, parsed *ParsedRequest, anthropicBody []byte, modelID, profileArn string) string {
	var anchorType, anchor string
	if parsed != nil {
		if explicitID := strings.TrimSpace(parsed.ExplicitSessionID); explicitID != "" {
			anchorType, anchor = "explicit", explicitID
		} else if metadataUserID := strings.TrimSpace(parsed.MetadataUserID); metadataUserID != "" {
			anchorType, anchor = "metadata", metadataUserID
		} else if systemText := extractTextFromSystemRaw(parsed.SystemRaw()); systemText != "" {
			anchorType, anchor = "system", systemText
		}
	}
	if anchor == "" && len(anthropicBody) > 0 {
		if systemText := extractTextFromSystemRaw([]byte(gjson.GetBytes(anthropicBody, "system").Raw)); systemText != "" {
			anchorType, anchor = "system", systemText
		} else if firstUserText := extractFirstUserText(anthropicBody); firstUserText != "" {
			anchorType, anchor = "first_user", firstUserText
		}
	}
	if anchor == "" {
		return ""
	}

	var sb strings.Builder
	_, _ = sb.WriteString("kiro-conversation-v1|")
	if account != nil {
		_, _ = sb.WriteString("account:")
		_, _ = sb.WriteString(strconv.FormatInt(account.ID, 10))
		_, _ = sb.WriteString("|credential:")
		_, _ = sb.WriteString(cacheCredentialIdentity(account))
		_, _ = sb.WriteString("|")
	}
	if parsed != nil && parsed.SessionContext != nil {
		_, _ = sb.WriteString("api_key:")
		_, _ = sb.WriteString(strconv.FormatInt(parsed.SessionContext.APIKeyID, 10))
		_, _ = sb.WriteString("|")
	}
	_, _ = sb.WriteString("model:")
	_, _ = sb.WriteString(strings.TrimSpace(modelID))
	_, _ = sb.WriteString("|profile:")
	_, _ = sb.WriteString(strings.TrimSpace(profileArn))
	_, _ = sb.WriteString("|anchor:")
	_, _ = sb.WriteString(anchorType)
	_, _ = sb.WriteString(":")
	_, _ = sb.WriteString(anchor)
	return sb.String()
}

func logKiroStatelessReplay(account *Account, payload []byte) {
	if account == nil {
		return
	}
	conversationID := gjson.GetBytes(payload, "conversationState.conversationId").String()
	systemPrompt := gjson.GetBytes(payload, "conversationState.history.0.userInputMessage.content").String()
	currentContent := gjson.GetBytes(payload, "conversationState.currentMessage.userInputMessage.content").String()
	logger.L().Info("kiro.stateless_replay",
		zap.Int64("selected_account_id", account.ID),
		zap.Bool("stateless_replay", true),
		zap.Int("history_count", len(gjson.GetBytes(payload, "conversationState.history").Array())),
		zap.Bool("has_agent_continuation_id", gjson.GetBytes(payload, "conversationState.agentContinuationId").Exists()),
		zap.String("conversation_id_hash", hashKiroLogString(conversationID)),
		zap.String("payload_hash_no_conversation_id", hashKiroPayloadWithoutConversationID(payload)),
		zap.String("system_prompt_hash", hashKiroLogString(systemPrompt)),
		zap.Int("system_prompt_len", len(systemPrompt)),
		zap.String("current_content_hash", hashKiroLogString(currentContent)),
		zap.Int("tool_count", len(gjson.GetBytes(payload, "conversationState.currentMessage.userInputMessage.userInputMessageContext.tools").Array())),
	)
}

// 体积守卫的通知响应头。
//
// G1-B 的症结不是"裁剪"本身，而是**静默**：请求被裁掉半部历史后照样返回 200，
// 调用方拿到一个自信但缺上下文的答案，完全无从察觉。日志只有服务端看得到，
// 所以必须在响应上留痕，让客户端/用户能发现这次回答是在残缺上下文上给出的。
const (
	kiroTrimHeaderTrimmed         = "x-sub2api-context-trimmed"
	kiroTrimHeaderDroppedItems    = "x-sub2api-context-dropped-items"
	kiroTrimHeaderCompressedItems = "x-sub2api-context-compressed-items"
	kiroTrimHeaderOriginalBytes   = "x-sub2api-context-original-bytes"
	kiroTrimHeaderFinalBytes      = "x-sub2api-context-final-bytes"
	kiroTrimHeaderStages          = "x-sub2api-context-stages"
)

// setKiroPayloadTrimHeaders 把守卫结果写进响应头。未触发时一个头都不加。
func setKiroPayloadTrimHeaders(header http.Header, requestCtx kiropkg.KiroRequestContext) {
	if header == nil {
		return
	}
	s := requestCtx.PayloadTrimStats()
	if !s.Triggered() {
		return
	}
	// trimmed=true 专指"真的丢了整轮历史"。仅压缩(内容仍在、只是被截断)
	// 与丢弃整轮是两种严重程度，不能混为一谈。
	header.Set(kiroTrimHeaderTrimmed, strconv.FormatBool(s.Trimmed))
	header.Set(kiroTrimHeaderDroppedItems, strconv.Itoa(s.DroppedItems))
	header.Set(kiroTrimHeaderCompressedItems, strconv.Itoa(s.CompressedItems))
	header.Set(kiroTrimHeaderOriginalBytes, strconv.Itoa(s.OriginalBytes))
	header.Set(kiroTrimHeaderFinalBytes, strconv.Itoa(s.FinalBytes))
	if len(s.Stages) > 0 {
		header.Set(kiroTrimHeaderStages, strings.Join(s.Stages, ","))
	}
}

// kiroTrimHeaderNames 是全部守卫通知头，供跨 http.Header 搬运时遍历。
var kiroTrimHeaderNames = []string{
	kiroTrimHeaderTrimmed,
	kiroTrimHeaderDroppedItems,
	kiroTrimHeaderCompressedItems,
	kiroTrimHeaderOriginalBytes,
	kiroTrimHeaderFinalBytes,
	kiroTrimHeaderStages,
}

// copyKiroTrimHeaders 把守卫通知头从 src 搬到 dst。
//
// 流式路径上这几个头是先写进 openKiroAnthropicStreamResponse 返回的
// resp.Header，再由 handleStreamingResponse 转发的；而那里走的是**上游头
// 白名单**，我们自己加的头一律被过滤掉。所以必须在调用前手工搬一次。
func copyKiroTrimHeaders(dst, src http.Header) {
	if dst == nil || src == nil {
		return
	}
	for _, name := range kiroTrimHeaderNames {
		if v := src.Get(name); v != "" {
			dst.Set(name, v)
		}
	}
}

// respondKiroPayloadTooLarge 把 behavior=reject 的守卫拒绝映射成 413。
//
// 返回 true 表示已写响应，调用方应立即返回、不要再走通用 502 分支。
//
// 抽成公共函数而不是各写一份：流式与非流式是两个独立入口，
// 只在其中一个加映射，另一个仍会回 502 —— 这正是 2026-09-14
// 交互式 CLI 实测踩到的坑（CLI 只走流式，curl 默认非流式，
// 单测断言的也是非流式，三者一致地掩盖了这个缺口）。
func respondKiroPayloadTooLarge(c *gin.Context, err error) bool {
	weight, limit, ok := kiroPayloadTooLargeDetail(err)
	if !ok {
		return false
	}
	c.JSON(http.StatusRequestEntityTooLarge, gin.H{
		"type": "error",
		"error": gin.H{
			"type":    "invalid_request_error",
			"message": kiroPayloadTooLargeMessage(weight, limit),
		},
	})
	return true
}

// kiroPayloadTooLargeDetail 判别守卫拒绝错误并取出加权值/阈值。
// 独立出来是为了让不 import kiro 包的 OpenAI 兼容入口也能复用同一判据。
func kiroPayloadTooLargeDetail(err error) (weight int, limit int, ok bool) {
	var tooLarge *kiropkg.ErrKiroPayloadTooLarge
	if !errors.As(err, &tooLarge) {
		return 0, 0, false
	}
	return tooLarge.Weight, tooLarge.Limit, true
}

// kiroPayloadTooLargeMessage 统一错误文案，避免三个入口各写一份而漂移。
func kiroPayloadTooLargeMessage(weight, limit int) string {
	return fmt.Sprintf(
		"Request payload too large: weighted size %d exceeds the configured limit %d. "+
			"Reduce conversation history, tool output, or attachments.",
		weight, limit)
}

// logKiroPayloadTrim 在体积守卫真正裁剪（或裁剪后仍超限）时写一条诊断日志。
//
// 未触发时完全静默 —— 绝大多数请求都走这条路径，不该产生噪声。
// 没有这条日志的话，「守卫是否真的生效」在线上无法观测：
// 裁剪成功与「负载本来就没超限」在外部表现完全一样（都是 200）。
func logKiroPayloadTrim(account *Account, requestCtx kiropkg.KiroRequestContext) {
	s := requestCtx.PayloadTrimStats()
	// DeferredToUpstream 不计入 Triggered()（守卫没动过负载，不该回裁剪头），
	// 但日志必须留 —— "超阈值却故意原样发"正是排查上游 400 时最需要的线索。
	if !s.Triggered() && !s.DeferredToUpstream {
		return
	}
	fields := []zap.Field{
		zap.Bool("compressed", s.Compressed),
		zap.Bool("trimmed", s.Trimmed),
		zap.Bool("still_oversized", s.StillOversized),
		zap.Bool("deferred_to_upstream", s.DeferredToUpstream),
		zap.Int("original_bytes", s.OriginalBytes),
		zap.Int("final_bytes", s.FinalBytes),
		// 加权值才是与阈值同口径的判据：上游限的不是字节数，
		// 同样字节的中文比英文"贵"得多。
		zap.Int("original_weight", s.OriginalWeight),
		zap.Int("final_weight", s.FinalWeight),
		zap.Int("limit_weight", s.LimitWeight),
		zap.Int("compressed_items", s.CompressedItems),
		zap.Int("dropped_history_items", s.DroppedItems),
		zap.Strings("stages", s.Stages),
	}
	if account != nil {
		fields = append(fields, zap.Int64("account_id", account.ID))
	}
	switch {
	case s.Rejected:
		logger.L().Warn("kiro.payload_rejected", fields...)
	case s.DeferredToUpstream:
		// on_upstream_400 行为下的既定动作：超阈值但故意原样发，等上游裁决。
		// 不能走 still_oversized 分支 —— 那是"缩到底仍塞不下"的软失败，
		// 混用会让这条策略下的每个大请求都误报成压缩失效。
		logger.L().Info("kiro.payload_deferred_to_upstream", fields...)
	case s.StillOversized:
		// 软失败：找不到干净切点或压缩+裁剪到底仍超限，照发不误，但必须可观测。
		logger.L().Warn("kiro.payload_still_oversized", fields...)
	case s.Trimmed:
		// 真的丢了整轮历史 —— 上下文已缺失，级别高于单纯压缩。
		logger.L().Warn("kiro.payload_trimmed", fields...)
	default:
		// 只压缩没裁剪：语义骨架保留，属正常降级。
		logger.L().Info("kiro.payload_compressed", fields...)
	}
}

func hashKiroPayloadWithoutConversationID(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	normalized := payload
	if next, err := sjson.DeleteBytes(payload, "conversationState.conversationId"); err == nil {
		normalized = next
	}
	return strconv.FormatUint(xxhash.Sum64(normalized), 36)
}

func hashKiroLogString(value string) string {
	if value == "" {
		return ""
	}
	return strconv.FormatUint(xxhash.Sum64String(value), 36)
}

func prepareKiroPayloadBodyForRequestModel(anthropicBody []byte, requestModel string) []byte {
	requestModel = strings.TrimSpace(requestModel)
	if requestModel == "" || !strings.Contains(strings.ToLower(requestModel), "thinking") {
		return anthropicBody
	}
	bodyModel := strings.TrimSpace(gjson.GetBytes(anthropicBody, "model").String())
	if bodyModel == "" || strings.EqualFold(bodyModel, requestModel) || strings.Contains(strings.ToLower(bodyModel), "thinking") {
		return anthropicBody
	}
	if next, ok := setJSONValueBytes(anthropicBody, "model", requestModel); ok {
		return next
	}
	return anthropicBody
}

func (s *GatewayService) markKiroAuthTemporarilyUnavailable(ctx context.Context, account *Account, statusCode int, body string) {
	if s == nil || s.accountRepo == nil || account == nil {
		return
	}
	until := time.Now().Add(10 * time.Minute)
	reason := fmt.Sprintf("kiro auth failure (%d): %s", statusCode, strings.TrimSpace(body))
	_ = s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, reason)
}

func (s *GatewayService) markKiroMonthlyRequestCountRateLimited(ctx context.Context, account *Account, body string) {
	if s == nil || s.accountRepo == nil || account == nil {
		return
	}
	resetAt := nextKiroMonthlyResetUTC(time.Now())
	if err := s.accountRepo.SetRateLimited(ctx, account.ID, resetAt); err != nil {
		logger.L().Warn("kiro monthly request count rate-limit failed",
			zap.Int64("account_id", account.ID),
			zap.Time("reset_at", resetAt),
			zap.Error(err),
		)
		return
	}
	reason := "kiro monthly request count exhausted (402): MONTHLY_REQUEST_COUNT"
	if trimmed := strings.TrimSpace(body); trimmed != "" {
		reason = fmt.Sprintf("%s body=%s", reason, truncateForLog([]byte(trimmed), 512))
	}
	logger.L().Warn("kiro monthly request count rate-limited",
		zap.Int64("account_id", account.ID),
		zap.Time("reset_at", resetAt),
		zap.String("reason", reason),
	)
}

func nextKiroMonthlyResetUTC(now time.Time) time.Time {
	utc := now.UTC()
	year, month, _ := utc.Date()
	return time.Date(year, month+1, 1, 0, 0, 0, 0, time.UTC)
}

func resetHTTPResponseBody(resp *http.Response, body []byte) {
	if resp == nil {
		return
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
}

func estimateKiroInputTokens(ctx context.Context, body []byte) int {
	if len(body) == 0 {
		return 0
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err == nil {
		return countKiroInputTokensFromPayload(ctx, payload)
	}
	tokens := len(body) / 4
	if tokens == 0 {
		return 1
	}
	return tokens
}

func kiroUsageToClaude(usage kiropkg.Usage, fallbackInput int) ClaudeUsage {
	inputTokens := usage.InputTokens
	if inputTokens == 0 {
		inputTokens = fallbackInput
	}
	return ClaudeUsage{
		InputTokens:              inputTokens,
		OutputTokens:             usage.OutputTokens,
		CacheReadInputTokens:     usage.CacheReadInputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		CacheCreation5mTokens:    usage.CacheCreation5mInputTokens,
		CacheCreation1hTokens:    usage.CacheCreation1hInputTokens,
		KiroCredits:              usage.KiroCredits,
	}
}

func (s *GatewayService) markKiroInvalidModelRateLimited(ctx context.Context, account *Account, mappedModel string) {
	if s == nil || s.accountRepo == nil || account == nil || account.Type != AccountTypeOAuth {
		return
	}
	resetAt := time.Now().Add(kiroInvalidModelTempUnschedDuration)
	if err := s.accountRepo.SetRateLimited(ctx, account.ID, resetAt); err != nil {
		logger.L().Warn("kiro invalid model rate-limit failed",
			zap.Int64("account_id", account.ID),
			zap.String("mapped_model", strings.TrimSpace(mappedModel)),
			zap.Time("reset_at", resetAt),
			zap.Error(err),
		)
		return
	}
	logger.L().Warn("kiro invalid model rate-limited",
		zap.Int64("account_id", account.ID),
		zap.String("mapped_model", strings.TrimSpace(mappedModel)),
		zap.Time("reset_at", resetAt),
	)
}

func (s *GatewayService) handleKiroHTTPError(ctx context.Context, resp *http.Response, c *gin.Context, account *Account, mappedModel string, requestBody []byte) error {
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
	upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
	if upstreamMsg == "" {
		upstreamMsg = strings.TrimSpace(string(respBody))
	}
	classification := classifyKiroHTTPError(resp.StatusCode, string(respBody))
	if resp.StatusCode == http.StatusBadRequest {
		logKiroBadRequestClassification(classification, account, "", resp.Header, respBody)
	}
	if classification.Category == kiroErrorMonthlyRequest {
		s.markKiroMonthlyRequestCountRateLimited(ctx, account, string(respBody))
	}
	if classification.Category == kiroErrorBadRequestInvalidModel && account != nil && account.Type == AccountTypeOAuth {
		s.markKiroInvalidModelRateLimited(ctx, account, mappedModel)
		event := s.buildKiroInvalidModelUpstreamEvent(account, resp, upstreamMsg, mappedModel, requestBody, c)
		appendOpsUpstreamError(c, event)
		return &UpstreamFailoverError{
			StatusCode:      resp.StatusCode,
			ResponseBody:    respBody,
			ResponseHeaders: resp.Header.Clone(),
		}
	}

	if resp.StatusCode == http.StatusPaymentRequired || s.shouldFailoverUpstreamError(resp.StatusCode) {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  buildKiroRequestID(resp),
			Kind:               "failover",
			Message:            upstreamMsg,
		})
		// 429 已经被 executeKiroUpstreamWithParsed → markKiro429 完整处理（Redis 1-5min
		// 指数退避 + DB rate_limit_reset_at 同步）。这里再走 HandleUpstreamError 会进入
		// handle429 → apply429FallbackRateLimit，把 DB cooldown 反写成 5s flat，
		// 直接抹掉我们刚算好的退避时长。所以 429 跳过通用 handler。
		if s.rateLimitService != nil && resp.StatusCode != http.StatusTooManyRequests {
			s.rateLimitService.HandleUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
		}
		return &UpstreamFailoverError{
			StatusCode:      resp.StatusCode,
			ResponseBody:    respBody,
			ResponseHeaders: resp.Header.Clone(),
		}
	}

	setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, "")
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID:  buildKiroRequestID(resp),
		Kind:               "http_error",
		Message:            upstreamMsg,
	})
	c.JSON(mapUpstreamStatusCode(resp.StatusCode), gin.H{
		"type": "error",
		"error": gin.H{
			"type":    claudeErrorType(resp.StatusCode),
			"message": coalesceKiroErrorMessage(resp.StatusCode, upstreamMsg),
		},
	})
	return fmt.Errorf("kiro upstream error: %d %s", resp.StatusCode, upstreamMsg)
}

func claudeErrorType(statusCode int) string {
	switch statusCode {
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable:
		return "overloaded_error"
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	default:
		return "api_error"
	}
}

func (s *GatewayService) buildKiroInvalidModelUpstreamEvent(account *Account, resp *http.Response, upstreamMsg, mappedModel string, requestBody []byte, c *gin.Context) OpsUpstreamErrorEvent {
	_ = s
	requestedModel := strings.TrimSpace(gjson.GetBytes(requestBody, "model").String())
	hasTools := gjson.GetBytes(requestBody, "tools").Exists()
	hasAdaptiveThinking := strings.EqualFold(strings.TrimSpace(gjson.GetBytes(requestBody, "thinking.type").String()), "adaptive")
	hasContext1MBeta := false
	if c != nil {
		hasContext1MBeta = strings.Contains(c.GetHeader("Anthropic-Beta"), "context-1m")
	}
	return OpsUpstreamErrorEvent{
		Platform:            account.Platform,
		AccountID:           account.ID,
		AccountName:         account.Name,
		UpstreamStatusCode:  resp.StatusCode,
		UpstreamRequestID:   buildKiroRequestID(resp),
		Kind:                "failover",
		Message:             upstreamMsg,
		RequestedModel:      requestedModel,
		MappedModel:         strings.TrimSpace(mappedModel),
		KiroModelID:         kiropkg.MapModel(mappedModel),
		HasTools:            hasTools,
		HasAdaptiveThinking: hasAdaptiveThinking,
		HasContext1MBeta:    hasContext1MBeta,
	}
}

func logKiroBadRequestClassification(classification kiroErrorClassification, account *Account, model string, headers http.Header, body []byte) {
	if classification.StatusCode != http.StatusBadRequest {
		return
	}
	var accountID int64
	if account != nil {
		accountID = account.ID
	}
	logger.L().Warn("kiro upstream bad request classified",
		zap.String("category", classification.Category),
		zap.Int("status", classification.StatusCode),
		zap.Int64("account_id", accountID),
		zap.String("model", strings.TrimSpace(model)),
		zap.String("request_id", headers.Get("x-request-id")),
		zap.String("body_excerpt", truncateForLog(body, 512)),
	)
}

// dumpKiro429ResponseForDebug captures the first 2KB of a Kiro 429 response body
// and the rate-limit-relevant headers, then restores resp.Body so the caller can
// still consume it. Used to investigate whether Kiro returns a reset-time field
// (e.g. nextDateReset) we should parse instead of falling back to fixed cooldown.
func dumpKiro429ResponseForDebug(resp *http.Response, accountID int64, endpointURL, endpointName string) {
	if resp == nil || resp.Body == nil {
		return
	}
	const maxBytes = 2048
	limited := io.LimitReader(resp.Body, maxBytes+1)
	sample, err := io.ReadAll(limited)
	if err != nil {
		logger.L().Warn("kiro.429_debug_read_failed",
			zap.Int64("account_id", accountID),
			zap.String("endpoint", endpointName),
			zap.Error(err),
		)
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(nil))
		return
	}
	truncated := false
	if len(sample) > maxBytes {
		sample = sample[:maxBytes]
		truncated = true
	}
	rest, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(append(append([]byte{}, sample...), rest...)))

	headers := map[string]string{}
	for k, v := range resp.Header {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "ratelimit") || strings.Contains(lk, "retry") || strings.Contains(lk, "reset") ||
			lk == "content-type" || lk == "x-amzn-requestid" || lk == "x-amzn-errortype" {
			headers[k] = strings.Join(v, ",")
		}
	}

	logger.L().Warn("kiro.429_raw_response",
		zap.Int64("account_id", accountID),
		zap.String("endpoint_url", endpointURL),
		zap.String("endpoint_name", endpointName),
		zap.String("content_type", resp.Header.Get("Content-Type")),
		zap.Any("relevant_headers", headers),
		zap.Int("body_bytes", len(sample)),
		zap.Bool("truncated", truncated),
		zap.String("body_sample", string(sample)),
	)
}

func coalesceKiroErrorMessage(statusCode int, upstreamMsg string) string {
	if upstreamMsg != "" {
		return upstreamMsg
	}
	switch statusCode {
	case http.StatusTooManyRequests:
		return "Rate limit exceeded"
	case http.StatusForbidden:
		return "Access denied"
	case http.StatusUnauthorized:
		return "Authentication failed"
	default:
		return "Upstream request failed"
	}
}
