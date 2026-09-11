// Package remoteproxy 让本部署的管理面接管另一套 sub2api 的数据。
//
// 场景：sub2api1 与 sub2api2 是两套独立部署，希望在 sub2api1 的界面上查看和管理
// sub2api2 的数据，且 sub2api2 可能是未经改造的原版部署。
//
// 设计要点：
//
//  1. 浏览器只访问 sub2api1 的同源地址，跨域由服务端转发消除。CORS 是浏览器依据
//     目标服务器响应头执行的运行时策略，无法在前端构建期规避；让请求同源是唯一
//     不依赖目标端配合的解法。
//
//  2. 目标端只认 x-api-key（原版 admin 中间件既有能力），因此 sub2api2 无需任何
//     改动。Admin API Key 由管理员在登录页提交，仅在登录时经由浏览器传输一次，
//     此后保存在服务端会话中，不落到浏览器存储。
//
//  3. 登录成功后签发一枚独立的随机会话令牌，只用于通过本组件的转发入口。
//     刻意不复用本地用户签发标准 JWT：那会让远程会话与某个本地管理员账号共用
//     刷新令牌家族（一侧撤销会踢掉另一侧），并让本意只读取远端数据的登录凭空
//     获得本部署的管理员权限。
//
//  4. 转发目标只来自启动配置（remote_proxy.backend_url），不接受请求指定，
//     因此不构成任意 URL 转发器，无 SSRF 面。
package remoteproxy

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"

	"github.com/gin-gonic/gin"
)

const (
	// 转发时携带的管理员认证头，与原版 admin 中间件一致。
	adminAPIKeyHeader = "X-API-Key"

	// 会话中保存 Admin API Key 的有效期。取值与 refresh token 家族无关，
	// 到期后需要重新登录，避免服务端长期驻留凭据。
	sessionTTL = 12 * time.Hour

	// 转发目标的默认超时。流式接口（如账号测试 SSE）依赖较长的响应时间。
	defaultTimeoutSeconds = 60
)

// hopByHopHeaders 是 RFC 7230 定义的逐跳首部，转发时必须剥离，
// 否则会破坏本端与目标端之间的连接语义。
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// Config 是校验后的运行配置。
type Config struct {
	BackendURL string
	Timeout    time.Duration
}

// Validate 校验并归一化配置。空 URL、非 http(s) 协议均视为配置错误，
// 在启动期直接失败优于运行期才暴露。
func Validate(cfg config.RemoteProxyConfig) (*Config, error) {
	raw := strings.TrimSpace(cfg.BackendURL)
	if raw == "" {
		return nil, errors.New("remote_proxy.enabled=true 但 remote_proxy.backend_url 为空")
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("remote_proxy.backend_url 不是合法 URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("remote_proxy.backend_url 必须是 http 或 https，当前为 %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, errors.New("remote_proxy.backend_url 缺少主机名")
	}

	// 只保留 scheme://host，路径前缀在转发时按请求路径拼接，
	// 避免配置里带 /api/v1 时出现重复前缀。
	base := &url.URL{Scheme: parsed.Scheme, Host: parsed.Host}

	timeout := time.Duration(cfg.Timeout) * time.Second
	if cfg.Timeout <= 0 {
		timeout = defaultTimeoutSeconds * time.Second
	}

	return &Config{BackendURL: base.String(), Timeout: timeout}, nil
}

// session 保存一次远程登录所对应的 Admin API Key。
type session struct {
	adminKey  string
	expiresAt time.Time
}

// Component 承载远程代管的登录与转发。
//
// 刻意不持有任何本地 service：远程会话与本部署的账号体系完全隔离，
// 这既避免了两侧会话互相干扰，也让本组件可独立于上游代码演进。
type Component struct {
	cfg *Config
	// verifyClient 用于登录时探测 Key，带整体超时。
	verifyClient *http.Client
	// proxyClient 用于转发，不设整体超时，否则 SSE 会在超时点被切断；
	// 连接建立仍受 Transport 层超时保护，请求生命周期由 gin 的 ctx 控制。
	proxyClient *http.Client

	mu       sync.RWMutex
	sessions map[string]*session
}

// New 构造组件。调用方需先通过 Validate 校验配置。
func New(cfg *Config) *Component {
	// 两个客户端共享同一 Transport，以复用连接池；仅超时语义不同。
	transport := http.DefaultTransport.(*http.Transport).Clone()

	// 不跟随重定向：目标端的 3xx 应原样回传给浏览器，
	// 由本端代为跟随会绕过调用方对目标地址的预期。
	noRedirect := func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	return &Component{
		cfg:          cfg,
		verifyClient: &http.Client{Transport: transport, Timeout: cfg.Timeout, CheckRedirect: noRedirect},
		proxyClient:  &http.Client{Transport: transport, CheckRedirect: noRedirect},
		sessions:     make(map[string]*session),
	}
}

// ==================== 登录 ====================

type loginRequest struct {
	AdminKey string `json:"admin_key" binding:"required"`
}

// loginResponse 只回传会话令牌。
//
// 刻意不回传目标地址：前端已能从 /remote/info 取得，重复下发只会多一处
// 需要同步的真实来源。过期时间也不回传——远程会话没有刷新令牌，
// 前端无从续期，提前告知反而会诱导实现无意义的续期逻辑。
type loginResponse struct {
	AccessToken string `json:"access_token"`
}

// Login 校验 Admin API Key 对目标端是否有效，通过后签发本地 JWT 会话。
// POST /api/v1/remote/login
func (p *Component) Login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	adminKey := strings.TrimSpace(req.AdminKey)
	if adminKey == "" {
		response.Unauthorized(c, "Invalid admin API key")
		return
	}

	// 用一个最轻量的管理接口探测 Key 是否被目标端接受。
	if err := p.verifyAdminKey(c.Request.Context(), adminKey); err != nil {
		if errors.Is(err, errInvalidKey) {
			response.Unauthorized(c, "Invalid admin API key")
			return
		}
		slog.Error("remote proxy: verify admin key failed", "error", err, "backend", p.cfg.BackendURL)
		response.InternalError(c, "Failed to reach remote backend")
		return
	}

	// 签发独立的远程会话令牌。
	//
	// 刻意不复用本地用户签发标准 JWT：那会让远程会话与某个本地管理员账号共用
	// 刷新令牌家族，导致两侧会话互相影响（一侧撤销会踢掉另一侧），并且让本意
	// 只读取远端数据的登录凭空获得本部署的管理员权限。远程会话只需能通过转发
	// 入口，不代表本部署的任何账号。
	token, err := p.newSession(adminKey)
	if err != nil {
		slog.Error("remote proxy: create session failed", "error", err)
		response.InternalError(c, "Failed to create session")
		return
	}

	response.Success(c, loginResponse{AccessToken: token})
}

var errInvalidKey = errors.New("remote backend rejected the admin api key")

// verifyAdminKey 以目标端的管理接口验证 Key。401/403 表示 Key 无效，
// 其余非 2xx 一律视为目标端不可用，避免把网络故障误报成凭据错误。
//
// 探测路径须是真实存在且开销最小的管理接口：/admin/dashboard 只是路由分组前缀，
// 直接请求会得到 404，不能用于判定凭据有效性。
func (p *Component) verifyAdminKey(ctx context.Context, adminKey string) error {
	target := p.cfg.BackendURL + "/api/v1/admin/dashboard/realtime"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("build verify request: %w", err)
	}
	req.Header.Set(adminAPIKeyHeader, adminKey)

	resp, err := p.verifyClient.Do(req)
	if err != nil {
		return fmt.Errorf("reach remote backend: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return errInvalidKey
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	default:
		return fmt.Errorf("remote backend returned %d", resp.StatusCode)
	}
}

// ==================== 会话 ====================

// newSession 生成远程会话令牌。令牌是高熵随机串，服务端按其查找 Admin API Key，
// 因此 Key 本身不会再次经过浏览器。
func (p *Component) newSession(adminKey string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(buf)

	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneLocked()
	p.sessions[token] = &session{adminKey: adminKey, expiresAt: time.Now().Add(sessionTTL)}

	return token, nil
}

// lookupSession 查找未过期的会话。令牌是 256 位随机值，无法通过猜测枚举，
// 因此按 map 直接查找即可。
func (p *Component) lookupSession(token string) (string, bool) {
	if token == "" {
		return "", false
	}

	p.mu.RLock()
	s, ok := p.sessions[token]
	p.mu.RUnlock()

	if !ok {
		return "", false
	}
	if time.Now().After(s.expiresAt) {
		p.mu.Lock()
		delete(p.sessions, token)
		p.mu.Unlock()
		return "", false
	}
	return s.adminKey, true
}

func (p *Component) revokeSession(token string) {
	p.mu.Lock()
	delete(p.sessions, token)
	p.mu.Unlock()
}

// pruneLocked 清除过期会话，避免长期运行下 map 无限增长。调用方须持有写锁。
func (p *Component) pruneLocked() {
	now := time.Now()
	for id, s := range p.sessions {
		if now.After(s.expiresAt) {
			delete(p.sessions, id)
		}
	}
}

// ==================== 转发 ====================

// Proxy 将管理面请求透传到目标端。
//
// 鉴权只依赖远程会话令牌本身：该令牌由 Login 签发，凭有效的 Admin API Key 换取，
// 因此持有它即等价于持有目标端的管理员凭据，无需再叠加本地账号体系。
func (p *Component) Proxy(c *gin.Context) {
	adminKey, ok := p.lookupSession(bearerToken(c))
	if !ok {
		response.Unauthorized(c, "Remote session expired")
		return
	}

	target := p.cfg.BackendURL + c.Request.URL.Path
	if raw := c.Request.URL.RawQuery; raw != "" {
		target += "?" + raw
	}

	// 不设超时上限，交由客户端超时与请求上下文控制，
	// 以免截断 SSE 等长连接响应。
	outReq, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, target, c.Request.Body)
	if err != nil {
		slog.Error("remote proxy: build request failed", "error", err, "path", c.Request.URL.Path)
		response.InternalError(c, "Failed to build upstream request")
		return
	}

	copyHeaders(outReq.Header, c.Request.Header)
	stripHopByHop(outReq.Header)

	// 远程会话令牌与本端 Cookie 都不应外泄给目标端；目标端只认 Admin API Key。
	outReq.Header.Del("Authorization")
	outReq.Header.Del("Cookie")
	outReq.Header.Set(adminAPIKeyHeader, adminKey)
	outReq.Header.Set("X-Forwarded-Host", c.Request.Host)

	// 流式响应不能受 client.Timeout 约束，否则 SSE 会在超时点被切断。
	resp, err := p.proxyClient.Do(outReq)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// 客户端主动断开，无需回写。
			return
		}
		slog.Error("remote proxy: upstream request failed", "error", err, "path", c.Request.URL.Path)
		response.InternalError(c, "Failed to reach remote backend")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	respHeader := c.Writer.Header()
	copyHeaders(respHeader, resp.Header)
	stripHopByHop(respHeader)
	// 目标端的 CORS 头对同源的浏览器请求无意义，且可能与本端策略冲突。
	respHeader.Del("Access-Control-Allow-Origin")
	respHeader.Del("Access-Control-Allow-Credentials")

	c.Writer.WriteHeader(resp.StatusCode)

	// 逐块拷贝并即时 flush，保证 SSE / 流式下载不被缓冲。
	flusher, canFlush := c.Writer.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := c.Writer.Write(buf[:n]); writeErr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				slog.Debug("remote proxy: upstream stream ended", "error", readErr, "path", c.Request.URL.Path)
			}
			return
		}
	}
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		for _, v := range values {
			dst.Add(key, v)
		}
	}
}

func stripHopByHop(h http.Header) {
	// Connection 首部自身可以列出额外的逐跳首部，需一并剥离。
	for _, name := range strings.Split(h.Get("Connection"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			h.Del(name)
		}
	}
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
}

// ==================== 路由注册 ====================

// bearerToken 从 Authorization 头取出令牌。前端沿用既有的 Bearer 携带方式，
// 因此无需为远程模式引入额外的请求头。
func bearerToken(c *gin.Context) string {
	parts := strings.SplitN(c.GetHeader("Authorization"), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// Logout 撤销远程会话，使服务端不再保留 Admin API Key。
// POST /api/v1/remote/logout
func (p *Component) Logout(c *gin.Context) {
	p.revokeSession(bearerToken(c))
	response.Success(c, gin.H{"success": true})
}

// Info 供前端在登录页判断本部署是否处于远程代管模式。
// GET /api/v1/remote/info
//
// 刻意不回传 backend_url：该接口无需鉴权（登录页要先探测才知道能否登录），
// 回传目标地址等于向任何匿名访问者公开被代管的是哪套部署。运维只需知道
// 本站处于代管模式即可，具体地址由部署方自己掌握。
func (p *Component) Info(c *gin.Context) {
	response.Success(c, gin.H{"enabled": true})
}

// Register 注册远程代管路由。仅在 remote_proxy.enabled 为真时调用。
//
// 路由刻意与既有 RegisterAdminRoutes 平行注册，不修改其内部结构，
// 以便上游合并时冲突面最小。
func Register(v1 *gin.RouterGroup, comp *Component) {
	v1.GET("/remote/info", comp.Info)
	v1.POST("/remote/login", comp.Login)
	v1.POST("/remote/logout", comp.Logout)

	// 转发全部管理面请求。鉴权在 Proxy 内完成（远程会话令牌）。
	v1.Any("/admin/*path", comp.Proxy)
}
