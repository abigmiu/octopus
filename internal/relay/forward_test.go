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

// setupRelayEnv 建立一个只包含指定渠道与分组的临时环境，供端到端转发用例使用。
func setupRelayEnv(t *testing.T, upstream string, mode model.GroupMode) (model.Channel, model.Group) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	resetSessions(t)

	if err := db.InitDB("sqlite", t.TempDir()+"/relay.db", false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}

	ctx := context.Background()
	channel := model.Channel{
		Name:        "upstream-a",
		Type:        model.ChannelProviderAnthropic,
		Enabled:     true,
		BaseURL:     upstream + "/v1##",
		Key:         "sk-test",
		CustomModel: "claude-real",
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	group := model.Group{Name: "octopus-sonnet", Mode: mode}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
	return channel, group
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

// 会话模式下首个请求必须阻塞等待人工选择，绑定建立后请求继续并把绑定的模型写给上游。
func TestForwardSessionModeBlocksUntilBound(t *testing.T) {
	seen := make(chan *http.Request, 1)
	upstream := anthropicUpstream(t, seen)
	defer upstream.Close()

	channel, _ := setupRelayEnv(t, upstream.URL, model.GroupModeSession)

	engine := gin.New()
	engine.POST("/v1/messages", Forward(llm.APIFormatAnthropicMessage))

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"octopus-sonnet","messages":[{"role":"user","content":"ping"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Claude-Code-Session-Id", "e2e-1")

	done := make(chan struct{})
	go func() {
		engine.ServeHTTP(recorder, request)
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
	if session.Usage.PromptTokens != 11 || session.Usage.CompletionTokens != 3 {
		t.Fatalf("session usage = %+v", session.Usage)
	}
	if session.RequestCount != 1 {
		t.Fatalf("request count = %d, want 1", session.RequestCount)
	}
}

// 同一会话的后续请求直接沿用已有绑定，不再等待人工选择。
func TestForwardSessionModeReusesBinding(t *testing.T) {
	seen := make(chan *http.Request, 2)
	upstream := anthropicUpstream(t, seen)
	defer upstream.Close()

	channel, _ := setupRelayEnv(t, upstream.URL, model.GroupModeSession)

	engine := gin.New()
	engine.POST("/v1/messages", Forward(llm.APIFormatAnthropicMessage))

	call := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
			`{"model":"octopus-sonnet","messages":[{"role":"user","content":"ping"}]}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Claude-Code-Session-Id", "e2e-2")
		engine.ServeHTTP(recorder, request)
		return recorder
	}

	// 先登记会话再绑定，使首个请求也无需等待。
	identity := identifySession(http.Header{"X-Claude-Code-Session-Id": {"e2e-2"}}, []byte(`{"messages":[]}`))
	registerSessionRequest(identity, "octopus-sonnet", 0, true)
	if err := SessionBind("claude:e2e-2", channel.ID, "claude-real"); err != nil {
		t.Fatalf("bind session: %v", err)
	}

	for i := 0; i < 2; i++ {
		finished := make(chan *httptest.ResponseRecorder, 1)
		go func() { finished <- call() }()
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

// 上游卡住时必须按分组配置的首响应时限失败，并给出超时原因而不是上下文取消。
func TestForwardReportsFirstResponseTimeout(t *testing.T) {
	blocked := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked
	}))
	defer upstream.Close()
	defer close(blocked)

	channel, group := setupRelayEnv(t, upstream.URL, model.GroupModeManual)

	// 手动模式需要指定成员，并把首响应时限压到 1 秒以便用例快速结束。
	updated, err := op.GroupUpdate(&model.GroupUpdateRequest{
		ID: group.ID,
		RelayConfig: &model.GroupRelayConfig{
			MemberMaxAttempts:                     1,
			MemberRetryIntervalSeconds:            1,
			MemberNonStreamResponseTimeoutSeconds: 1,
			MemberStreamFirstEventTimeoutSeconds:  1,
			MemberCooldownSeconds:                 1,
		},
		ItemsToAdd: []model.GroupItemAddRequest{{ChannelID: channel.ID, ModelName: "claude-real", Priority: 1}},
	}, context.Background())
	if err != nil {
		t.Fatalf("update group: %v", err)
	}
	itemID := updated.Items[0].ID
	if _, err := op.GroupActiveItemUpdate(group.ID, &model.GroupActiveItemUpdateRequest{ItemID: &itemID}, context.Background()); err != nil {
		t.Fatalf("set active item: %v", err)
	}

	engine := gin.New()
	engine.POST("/v1/messages", Forward(llm.APIFormatAnthropicMessage))

	recorder := httptest.NewRecorder()
	// 手动模式会一直重试当前成员直到客户端放弃, 因此用例以客户端超时结束请求。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"octopus-sonnet","messages":[{"role":"user","content":"ping"}]}`)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Claude-Code-Session-Id", "e2e-timeout")

	done := make(chan struct{})
	go func() {
		engine.ServeHTTP(recorder, request)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("request never ended despite the response timeout")
	}

	// 每一轮的失败原因必须保留下来，且指向上游超时而不是 context canceled。
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
	// 至少有一轮以上游超时结束; 客户端放弃时结束的那一轮记为客户端断开。
	timedOut := false
	for _, round := range state.Rounds {
		if strings.Contains(round.Error, "did not respond within") {
			timedOut = true
		}
	}
	if !timedOut {
		t.Fatalf("no round reported an upstream timeout, rounds = %+v", state.Rounds)
	}
	if !strings.Contains(state.Error, "did not respond within") {
		t.Fatalf("request error = %q, want the real failure reason to be kept", state.Error)
	}
	// 客户端放弃导致的取消原因单独记录, 不得覆盖上面的真实失败原因。
	if state.Status != StatusCanceled || state.CancelReason != clientCanceledReason {
		t.Fatalf("status = %q cancel reason = %q", state.Status, state.CancelReason)
	}
}
