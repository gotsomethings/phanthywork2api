package upstream

// oauthflow.go — P3：面板内添加账号（PKCE 授权链接 + 粘贴 code 换取 token）。
//
// 与 cmd/login 的两步式流程等价，但：
//   - verifier 存在内存 map（带 TTL），不再写 .login-verifier 文件，
//     避免多用户/并发相互覆盖；
//   - 交换成功后直接原子落盘 auths/，返回 *auth.Auth 供调用方加入池。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"phanthycode2api/internal/auth"
)

const (
	// OAuthRedirectURI 官方 CLI 使用的回调地址（固定值）。
	OAuthRedirectURI = "https://code.phanthy.com/oauth/code/success"
	// OAuthScope 与官方 CLI 对齐。
	OAuthScope = "user:inference user:profile user:sessions:claude_code"
	// loginSessionTTL 授权链接有效期，超时后需重新生成。
	loginSessionTTL = 15 * time.Minute
)

// pendingLogin 保存一次待完成的 PKCE 登录。
type pendingLogin struct {
	Verifier  string
	CreatedAt time.Time
}

// loginSessions 进程内 PKCE 会话表（支持并发多账号添加）。
var loginSessions = struct {
	sync.RWMutex
	m map[string]*pendingLogin
}{m: map[string]*pendingLogin{}}

// StartLogin 生成 PKCE verifier + 授权 URL，返回 (state, authURL, err)。
// 调用方把 state 回传给 ExchangeLogin 完成交换。
func (c *Client) StartLogin() (string, string, error) {
	verifier, err := randomString(48)
	if err != nil {
		return "", "", fmt.Errorf("generate verifier: %w", err)
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	state, err := randomString(24)
	if err != nil {
		return "", "", fmt.Errorf("generate state: %w", err)
	}

	cleanupLoginSessions()
	loginSessions.Lock()
	loginSessions.m[state] = &pendingLogin{Verifier: verifier, CreatedAt: time.Now()}
	loginSessions.Unlock()

	q := url.Values{}
	q.Set("client_id", c.ClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", OAuthRedirectURI)
	q.Set("scope", OAuthScope)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	authURL := strings.TrimRight(c.BaseAPIURL, "/") + "/oauth/authorize?" + q.Encode()
	return state, authURL, nil
}

// ExchangeLogin 用 state 找回 verifier，用 code 换取 token + api_key，
// 构造 *auth.Auth 并原子落盘 auths/<uid>.json。
// code 支持裸 code 或完整回调 URL；proxy 可为空（走全局默认）。
func (c *Client) ExchangeLogin(state, code, authDir, proxy, nickname string) (*auth.Auth, error) {
	code = extractOAuthCode(code)
	if code == "" {
		return nil, fmt.Errorf("empty authorization code")
	}

	cleanupLoginSessions()
	loginSessions.Lock()
	pend, ok := loginSessions.m[state]
	if ok {
		delete(loginSessions.m, state)
	}
	loginSessions.Unlock()
	if !ok || pend == nil {
		return nil, fmt.Errorf("login session not found or expired; regenerate the authorization link")
	}

	tok, err := c.exchangeCode(code, pend.Verifier, proxy)
	if err != nil {
		return nil, err
	}

	// api_key 失败不致命：上游 prod 该端点可能 404，运行时用 access_token 兜底。
	apiKey, keyErr := c.CreateAPIKeyRaw(tok.AccessToken, proxy)
	if keyErr != nil {
		apiKey = ""
	}

	uid := fmt.Sprintf("phanthy-%d", time.Now().Unix())
	if strings.TrimSpace(nickname) == "" {
		nickname = "phanthy-" + time.Now().Format("0102")
	}

	filePath, err := auth.NewFilePath(authDir, uid)
	if err != nil {
		return nil, fmt.Errorf("prepare auth dir: %w", err)
	}

	a := &auth.Auth{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
		APIKey:       apiKey,
		UID:          uid,
		Nickname:     nickname,
		Proxy:        strings.TrimSpace(proxy),
		FilePath:     filePath,
	}
	if err := a.SaveAtomic(); err != nil {
		return nil, fmt.Errorf("save auth file: %w", err)
	}
	return a, nil
}

// CancelLogin 丢弃一个待完成会话（用户取消）。
func (c *Client) CancelLogin(state string) {
	loginSessions.Lock()
	delete(loginSessions.m, state)
	loginSessions.Unlock()
}

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

func cleanupLoginSessions() {
	deadline := time.Now().Add(-loginSessionTTL)
	loginSessions.Lock()
	for k, v := range loginSessions.m {
		if v.CreatedAt.Before(deadline) {
			delete(loginSessions.m, k)
		}
	}
	loginSessions.Unlock()
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// extractOAuthCode 从用户输入中提取 code（支持裸 code 或完整 URL）。
func extractOAuthCode(s string) string {
	s = strings.TrimSpace(s)
	if u, err := url.Parse(s); err == nil && u.Scheme != "" {
		if c := u.Query().Get("code"); c != "" {
			return c
		}
	}
	if i := strings.Index(s, "code="); i >= 0 {
		s = s[i+5:]
		if j := strings.IndexAny(s, "& \t\r\n"); j >= 0 {
			s = s[:j]
		}
	}
	return strings.TrimSpace(s)
}

type oauthTokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func (c *Client) exchangeCode(code, verifier, proxy string) (*oauthTokenResp, error) {
	form := map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"code_verifier": verifier,
		"redirect_uri":  OAuthRedirectURI,
		"client_id":     c.ClientID,
	}
	extra := map[string]string{"anthropic-beta": c.OAuthBetaHeader}
	m, err := c.formPost(c.OAuthTokenURL, form, extra, c.effectiveProxy(proxy))
	if err != nil {
		return nil, err
	}
	tok := &oauthTokenResp{}
	if v, ok := m["access_token"].(string); ok {
		tok.AccessToken = v
	}
	if v, ok := m["refresh_token"].(string); ok {
		tok.RefreshToken = v
	}
	if v, ok := toInt64(m["expires_in"]); ok {
		tok.ExpiresIn = v
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("exchange failed: no access_token in response")
	}
	if tok.ExpiresIn <= 0 {
		tok.ExpiresIn = 3600
	}
	return tok, nil
}

// CreateAPIKeyRaw 用给定 access_token 创建 api_key（不依赖 *auth.Auth，供登录流程用）。
// 上游 404 时返回 ("", nil)，表示“不可用但非错误”。
func (c *Client) CreateAPIKeyRaw(accessToken, proxy string) (string, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return "", fmt.Errorf("no accessToken")
	}
	req, err := http.NewRequest(http.MethodPost, c.APIKeyURL, nil)
	if err != nil {
		return "", err
	}
	o := c.Opts()
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", c.OAuthBetaHeader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", o.UserAgent)
	for k, v := range o.ExtraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := c.doWithProxyControl(c.effectiveProxy(proxy), req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		if resp.StatusCode == http.StatusNotFound {
			return "", nil
		}
		kind := Classify(resp.StatusCode, string(raw))
		return "", &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", nil
	}
	if d, ok := m["data"].(map[string]any); ok {
		for _, k := range []string{"raw_key", "api_key", "apiKey"} {
			if v, ok := d[k].(string); ok && v != "" {
				return v, nil
			}
		}
	}
	for _, k := range []string{"api_key", "apiKey", "raw_key"} {
		if v, ok := m[k].(string); ok && v != "" {
			return v, nil
		}
	}
	return "", nil
}
