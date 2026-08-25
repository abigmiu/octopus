package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/llm"
)

// setupRelayEnv 建立一个只包含指定渠道的临时环境，供端到端转发用例使用。
func setupRelayEnv(t *testing.T, upstream string) model.Channel {
	t.Helper()
	gin.SetMode(gin.TestMode)
	resetSessions(t)

	if err := db.InitDB("sqlite", t.TempDir()+"/relay.db", false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}

	channel := model.Channel{
		Name:        "upstream-a",
		Type:        model.ChannelProviderAnthropic,
		Enabled:     true,
		BaseURL:     upstream + "/v1##",
		Key:         "sk-test",
		CustomModel: "claude-real",
	}
	if err := op.ChannelCreate(&channel, context.Background()); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	return channel
}

// relayConfigForTest 收紧全局转发配置，使超时用例能快速结束。
func relayConfigForTest(t *testing.T, attempts, retrySeconds, timeoutSeconds int) {
	t.Helper()
	for key, value := range map[model.SettingKey]int{
		model.SettingKeyRelayMaxAttempts:        attempts,
		model.SettingKeyRelayRetryInterval:      retrySeconds,
		model.SettingKeyRelayResponseTimeout:    timeoutSeconds,
		model.SettingKeyRelayStreamFirstTimeout: timeoutSeconds,
	} {
		if err := op.SettingSetInt(key, value); err != nil {
			t.Fatalf("set %s: %v", key, err)
		}
	}
}

// anthropicUpstream 返回一个最小的 Anthropic 非流式上游。
func anthropicUpstream(t *testing.T, seen chan<- *http.Request) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clone := r.Clone(context.Background())
		body, _ := io.ReadAll(r.Body)
		clone.Body = io.NopCloser(strings.NewReader(string(body)))
		select {
		case seen <- clone:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-real",
			"content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":11,"output_tokens":3}}`))
	}))
}

// claudeRequest 构造一条 Claude Code 风格的请求。
func claudeRequest(sessionID, prompt string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"claude-opus-5","messages":[{"role":"user","content":"`+prompt+`"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Claude-Code-Session-Id", sessionID)
	return request
}

// 新会话必须阻塞等待人工选择，绑定建立后请求继续并把绑定的模型写给上游。
func TestForwardBlocksUntilBound(t *testing.T) {
	seen := make(chan *http.Request, 1)
	upstream := anthropicUpstream(t, seen)
	defer upstream.Close()

	channel := setupRelayEnv(t, upstream.URL)

	engine := gin.New()
	engine.POST("/v1/messages", Forward(llm.APIFormatAnthropicMessage))

	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		engine.ServeHTTP(recorder, claudeRequest("e2e-1", "ping"))
		close(done)
	}()

	// 等待该会话出现并进入待选状态。
	deadline := time.Now().Add(3 * time.Second)
	for {
		sessionMu.Lock()
		session := sessions["claude:e2e-1"]
		pending := session != nil && session.PendingCount == 1 && session.Status == SessionStatusPending
		sessionMu.Unlock()
		if pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session never entered pending state")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("request completed before any channel was selected")
	default:
	}

	if err := SessionBind("claude:e2e-1", channel.ID, "claude-real"); err != nil {
		t.Fatalf("bind session: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not complete after binding")
	}

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var upstreamRequest *http.Request
	select {
	case upstreamRequest = <-seen:
	default:
		t.Fatal("upstream was never called")
	}
	body, _ := io.ReadAll(upstreamRequest.Body)
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	// 客户端请求的是 claude-opus-5，实际必须打到绑定的模型上。
	if payload.Model != "claude-real" {
		t.Fatalf("upstream model = %q, want the bound model", payload.Model)
	}
	// 同协议透传必须保留客户端请求头，依赖客户端校验的上游才能通过。
	if got := upstreamRequest.Header.Get("X-Claude-Code-Session-Id"); got != "e2e-1" {
		t.Fatalf("client header was not forwarded, got %q", got)
	}

	sessionMu.Lock()
	defer sessionMu.Unlock()
	session := sessions["claude:e2e-1"]
	if session.Status != SessionStatusBound || session.PendingCount != 0 {
		t.Fatalf("session = %+v", session)
	}
	if session.RequestedModel != "claude-opus-5" {
		t.Fatalf("requested model = %q", session.RequestedModel)
	}
	if session.Usage.PromptTokens != 11 || session.Usage.CompletionTokens != 3 {
		t.Fatalf("session usage = %+v", session.Usage)
	}
}

// 同一会话的后续请求直接沿用已有绑定，不再等待人工选择。
func TestForwardReusesBinding(t *testing.T) {
	seen := make(chan *http.Request, 2)
	upstream := anthropicUpstream(t, seen)
	defer upstream.Close()

	channel := setupRelayEnv(t, upstream.URL)

	engine := gin.New()
	engine.POST("/v1/messages", Forward(llm.APIFormatAnthropicMessage))

	// 先登记会话再绑定，使首个请求也无需等待。
	identity := identifySession(http.Header{"X-Claude-Code-Session-Id": {"e2e-2"}}, []byte(`{"messages":[]}`))
	registerSessionRequest(identity, "claude-opus-5", 0)
	if err := SessionBind("claude:e2e-2", channel.ID, "claude-real"); err != nil {
		t.Fatalf("bind session: %v", err)
	}

	for i := 0; i < 2; i++ {
		finished := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, claudeRequest("e2e-2", "ping"))
			finished <- recorder
		}()
		select {
		case recorder := <-finished:
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("request %d blocked despite an existing binding", i+1)
		}
	}

	sessionMu.Lock()
	defer sessionMu.Unlock()
	if got := sessions["claude:e2e-2"].RequestCount; got != 3 {
		t.Fatalf("request count = %d, want 3", got)
	}
}

// 派生不出会话身份的请求必须被拒绝，不能沿用别的会话的绑定。
func TestForwardRejectsUnidentifiableSession(t *testing.T) {
	seen := make(chan *http.Request, 1)
	upstream := anthropicUpstream(t, seen)
	defer upstream.Close()

	setupRelayEnv(t, upstream.URL)

	engine := gin.New()
	engine.POST("/v1/messages", Forward(llm.APIFormatAnthropicMessage))

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-5"}`))
	request.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "identify a client session") {
		t.Fatalf("body = %s, want an explicit reason", recorder.Body.String())
	}
	select {
	case <-seen:
		t.Fatal("upstream must not be called for an unidentifiable session")
	default:
	}
}

// 上游卡住时必须按全局配置的首响应时限失败，并给出超时原因而不是上下文取消。
func TestForwardReportsFirstResponseTimeout(t *testing.T) {
	blocked := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked
	}))
	defer upstream.Close()
	defer close(blocked)

	channel := setupRelayEnv(t, upstream.URL)
	relayConfigForTest(t, 1, 1, 1)

	identity := identifySession(http.Header{"X-Claude-Code-Session-Id": {"e2e-timeout"}}, []byte(`{"messages":[]}`))
	registerSessionRequest(identity, "claude-opus-5", 0)
	if err := SessionBind("claude:e2e-timeout", channel.ID, "claude-real"); err != nil {
		t.Fatalf("bind session: %v", err)
	}

	engine := gin.New()
	engine.POST("/v1/messages", Forward(llm.APIFormatAnthropicMessage))

	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		engine.ServeHTTP(recorder, claudeRequest("e2e-timeout", "ping"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("request never ended despite the response timeout")
	}

	// 失败原因必须指向上游超时，而不是 context canceled。
	mu.Lock()
	defer mu.Unlock()
	var state *RequestState
	for _, candidate := range requests {
		if candidate.SessionID == "claude:e2e-timeout" {
			state = candidate
		}
	}
	if state == nil {
		t.Fatal("request state was not recorded")
	}
	if len(state.Rounds) == 0 {
		t.Fatal("no round was recorded")
	}
	if !strings.Contains(state.Error, "did not respond within") {
		t.Fatalf("request error = %q, want an upstream timeout", state.Error)
	}
	if state.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", state.Status)
	}
	// 绑定必须保留，等待人工换目标。
	sessionMu.Lock()
	defer sessionMu.Unlock()
	session := sessions["claude:e2e-timeout"]
	if session.ChannelID != channel.ID || session.Status != SessionStatusError {
		t.Fatalf("session = %+v, want the binding kept and marked as error", session)
	}
}
