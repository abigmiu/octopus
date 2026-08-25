package relay

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/looplj/axonhub/llm"
)

// resetSessions 清空进程内会话状态，使各用例互不干扰。
func resetSessions(t *testing.T) {
	t.Helper()
	sessionMu.Lock()
	sessions = make(map[string]*SessionState)
	sessionMu.Unlock()
}

func TestIdentifySessionExplicitSignals(t *testing.T) {
	cases := []struct {
		name    string
		headers http.Header
		body    string
		want    string
		client  string
	}{
		{
			name:    "claude code header",
			headers: http.Header{"X-Claude-Code-Session-Id": {"abc-123"}},
			body:    `{"messages":[{"role":"user","content":"hi"}]}`,
			want:    "claude:abc-123",
			client:  SessionClientClaudeCode,
		},
		{
			name:    "claude metadata json",
			headers: http.Header{},
			body:    `{"metadata":{"user_id":"{\"session_id\":\"s-1\"}"},"messages":[{"role":"user","content":"hi"}]}`,
			want:    "claude:s-1",
			client:  SessionClientClaudeCode,
		},
		{
			name:    "claude metadata legacy suffix",
			headers: http.Header{},
			body:    `{"metadata":{"user_id":"user_abc_session_2f1e8c04-aaaa-bbbb-cccc-000000000001"}}`,
			want:    "claude:2f1e8c04-aaaa-bbbb-cccc-000000000001",
			client:  SessionClientClaudeCode,
		},
		{
			name:    "codex header",
			headers: http.Header{"Session-Id": {"codex-9"}},
			body:    `{"input":"hello"}`,
			want:    "codex:codex-9",
			client:  SessionClientCodex,
		},
		{
			name:    "prompt cache key",
			headers: http.Header{},
			body:    `{"prompt_cache_key":"pck-7","input":"hello"}`,
			want:    "pck:pck-7",
			client:  SessionClientUnknown,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			identity := identifySession(testCase.headers, []byte(testCase.body))
			if identity.id != testCase.want {
				t.Fatalf("session id = %q, want %q", identity.id, testCase.want)
			}
			if identity.client != testCase.client {
				t.Fatalf("client = %q, want %q", identity.client, testCase.client)
			}
		})
	}
}

// 内容哈希兜底时，同一会话的首轮与后续轮必须派生出同一个标识，且后续轮能直接取到首轮建立的绑定。
// 这里覆盖过的回归是：标识在轮次之间跳变，导致后续轮查不到会话而报 "session ... not found"，
// 同一个会话在界面上也被劈成两张卡片。
func TestIdentifySessionContentHashAcrossTurns(t *testing.T) {
	resetSessions(t)

	first := identifySession(http.Header{}, []byte(`{"messages":[
		{"role":"system","content":"you are a helper"},
		{"role":"user","content":"first question"}]}`))
	if first.id == "" {
		t.Fatal("first turn produced no session id")
	}

	second := identifySession(http.Header{}, []byte(`{"messages":[
		{"role":"system","content":"you are a helper"},
		{"role":"user","content":"first question"},
		{"role":"assistant","content":"an answer"},
		{"role":"user","content":"second question"}]}`))
	if second.id != first.id {
		t.Fatalf("second turn id = %q, want the same as the first turn %q", second.id, first.id)
	}

	// 首轮登记并绑定，后续轮必须无需等待就取到同一个目标。
	registerSessionRequest(first, "claude-opus-5", 1)
	sessionMu.Lock()
	session := sessions[first.id]
	session.ChannelID = 9
	session.ChannelName = "channel-9"
	session.ModelName = "real-model"
	session.Status = SessionStatusBound
	sessionMu.Unlock()

	registerSessionRequest(second, "claude-opus-5", 2)
	target, ok, err := awaitSessionTarget(context.Background(), second)
	if !ok || err != nil {
		t.Fatalf("second turn could not resolve its session: ok=%v err=%v", ok, err)
	}
	if target.channelID != 9 || target.modelName != "real-model" {
		t.Fatalf("target = %+v, want the binding from the first turn", target)
	}

	sessionMu.Lock()
	defer sessionMu.Unlock()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	if got := sessions[first.id].RequestCount; got != 2 {
		t.Fatalf("request count = %d, want 2", got)
	}
}

// 备用标识与主标识必须命中同一条会话状态，且任一标识都能查到绑定。
func TestSessionAliasResolvesToSameSession(t *testing.T) {
	resetSessions(t)

	// prompt_cache_key 与 conversation 互为别名，首次请求同时携带两者。
	both := identifySession(http.Header{}, []byte(`{"prompt_cache_key":"pck-1","conversation":{"id":"conv-1"},"messages":[
		{"role":"user","content":"hello"}]}`))
	if both.id != "pck:pck-1" || both.alias != "conv:conv-1" {
		t.Fatalf("identity = %+v", both)
	}
	registerSessionRequest(both, "claude-opus-5", 1)
	if err := bindForTest(both.id, 4, "m"); err != nil {
		t.Fatalf("bind: %v", err)
	}

	// 后续请求只携带 conversation，必须命中同一会话而不是新建一个。
	onlyConversation := identifySession(http.Header{}, []byte(`{"conversation":{"id":"conv-1"},"messages":[
		{"role":"user","content":"hello"}]}`))
	if onlyConversation.id != "conv:conv-1" {
		t.Fatalf("identity = %+v", onlyConversation)
	}
	registerSessionRequest(onlyConversation, "claude-opus-5", 2)

	target, ok, err := awaitSessionTarget(context.Background(), onlyConversation)
	if !ok || err != nil {
		t.Fatalf("alias did not resolve: ok=%v err=%v", ok, err)
	}
	if target.channelID != 4 {
		t.Fatalf("target = %+v", target)
	}

	sessionMu.Lock()
	defer sessionMu.Unlock()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
}

// 会话记账被清空后，请求必须退化为重新等待选择，而不是直接失败。
func TestAwaitSessionTargetRecreatesClearedSession(t *testing.T) {
	resetSessions(t)

	identity := sessionIdentity{id: "claude:gone", label: "已清空", client: SessionClientClaudeCode}
	registerSessionRequest(identity, "claude-opus-5", 1)
	SessionClear()

	sessionMu.Lock()
	cleared := len(sessions) == 0
	sessionMu.Unlock()
	if !cleared {
		t.Fatal("session was not cleared")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, ok, err := awaitSessionTarget(ctx, identity)
	if ok {
		t.Fatal("await must not report a binding for a cleared session")
	}
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the wait to end on the client context, not a missing session", err)
	}

	sessionMu.Lock()
	defer sessionMu.Unlock()
	session := sessions[identity.id]
	if session == nil {
		t.Fatal("await should have recreated the session as pending")
	}
	if session.Status != SessionStatusPending || session.Label != "已清空" {
		t.Fatalf("session = %+v", session)
	}
}

// 标签取首条用户消息，兼容 Anthropic 的分段内容。
func TestSessionLabelFromSegmentedContent(t *testing.T) {
	label := sessionLabel(SessionClientClaudeCode, []byte(`{"system":"sys","messages":[
		{"role":"user","content":[{"type":"text","text":"帮我看一下这个报错"}]}]}`))
	if label != "帮我看一下这个报错" {
		t.Fatalf("label = %q", label)
	}
}

// Responses 协议的 instructions 与 input 项也要能派生出稳定标识。
func TestConversationHeadResponsesFormat(t *testing.T) {
	system, user, assistant := conversationHead([]byte(`{
		"instructions":"be brief",
		"input":[
			{"type":"reasoning","summary":[]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"ping"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"pong"}]}]}`))
	if system != "be brief" || user != "ping" || assistant != "pong" {
		t.Fatalf("head = (%q, %q, %q)", system, user, assistant)
	}
}

// 未绑定的会话必须阻塞等待人工选择，绑定建立后立即返回该目标。
func TestAwaitSessionTargetUnblocksOnBind(t *testing.T) {
	resetSessions(t)

	identity := sessionIdentity{id: "claude:wait-1", label: "等待中", client: SessionClientClaudeCode}
	registerSessionRequest(identity, "claude-opus-5", 1)

	type outcome struct {
		target sessionTarget
		ok     bool
	}
	done := make(chan outcome, 1)
	go func() {
		target, ok, _ := awaitSessionTarget(context.Background(), identity)
		done <- outcome{target: target, ok: ok}
	}()

	// 等待请求进入待选状态后再建立绑定。
	deadline := time.Now().Add(2 * time.Second)
	for {
		sessionMu.Lock()
		pending := sessions[identity.id].PendingCount
		sessionMu.Unlock()
		if pending == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request never entered pending state")
		}
		time.Sleep(5 * time.Millisecond)
	}

	sessionMu.Lock()
	session := sessions[identity.id]
	session.ChannelID = 7
	session.ChannelName = "channel-7"
	session.ModelName = "gpt-5"
	session.Status = SessionStatusBound
	for waiter := range session.waiters {
		close(waiter)
		delete(session.waiters, waiter)
	}
	sessionMu.Unlock()

	select {
	case result := <-done:
		if !result.ok {
			t.Fatal("await returned not ok after bind")
		}
		if result.target.channelID != 7 || result.target.modelName != "gpt-5" {
			t.Fatalf("target = %+v", result.target)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("await did not return after bind")
	}

	sessionMu.Lock()
	defer sessionMu.Unlock()
	if got := sessions[identity.id].PendingCount; got != 0 {
		t.Fatalf("pending count = %d, want 0", got)
	}
}

// 客户端断开时等待必须立即结束，且不留下待选计数。
func TestAwaitSessionTargetClientCanceled(t *testing.T) {
	resetSessions(t)

	identity := sessionIdentity{id: "claude:cancel-1", client: SessionClientClaudeCode}
	registerSessionRequest(identity, "claude-opus-5", 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := awaitSessionTarget(ctx, identity)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error after client cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("await did not return after client cancel")
	}

	sessionMu.Lock()
	defer sessionMu.Unlock()
	if got := sessions[identity.id].PendingCount; got != 0 {
		t.Fatalf("pending count = %d, want 0", got)
	}
}

// 会话数超出上限时淘汰最旧的空闲会话，仍在等待选择的会话必须保留。
func TestTrimSessionsKeepsPending(t *testing.T) {
	resetSessions(t)

	pending := sessionIdentity{id: "claude:pending", client: SessionClientClaudeCode}
	registerSessionRequest(pending, "claude-opus-5", 1)
	sessionMu.Lock()
	sessions[pending.id].PendingCount = 1
	sessions[pending.id].LastActiveAt = time.Now().Add(-time.Hour)
	sessionMu.Unlock()

	for i := 0; i < maxSessions+3; i++ {
		identity := sessionIdentity{id: "claude:bulk-" + string(rune('a'+i)), client: SessionClientClaudeCode}
		registerSessionRequest(identity, "claude-opus-5", uint64(100+i))
	}

	sessionMu.Lock()
	defer sessionMu.Unlock()
	if len(sessions) > maxSessions+1 {
		t.Fatalf("sessions = %d, want at most %d", len(sessions), maxSessions+1)
	}
	if sessions[pending.id] == nil {
		t.Fatal("pending session was evicted")
	}
}

// 会话用量按请求累加，失败原因写入后由下一次成功清除。
func TestSessionUsageAndFailure(t *testing.T) {
	resetSessions(t)

	identity := sessionIdentity{id: "claude:usage-1", client: SessionClientClaudeCode}
	registerSessionRequest(identity, "claude-opus-5", 1)
	sessionMu.Lock()
	sessions[identity.id].ChannelID = 3
	sessions[identity.id].ModelName = "m"
	sessionMu.Unlock()

	recordSessionFailure(identity.id, "upstream 500")
	sessionMu.Lock()
	if sessions[identity.id].Status != SessionStatusError {
		t.Fatalf("status = %q, want error", sessions[identity.id].Status)
	}
	sessionMu.Unlock()

	usage := &llm.Usage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14}
	recordSessionSuccess(identity.id, usage, statsMetricsForTest(0.5))
	recordSessionSuccess(identity.id, usage, statsMetricsForTest(0.25))

	sessionMu.Lock()
	defer sessionMu.Unlock()
	session := sessions[identity.id]
	if session.Usage.PromptTokens != 20 || session.Usage.CompletionTokens != 8 {
		t.Fatalf("usage = %+v", session.Usage)
	}
	if session.Cost != 0.75 {
		t.Fatalf("cost = %v, want 0.75", session.Cost)
	}
	if session.Status != SessionStatusBound || session.Error != "" {
		t.Fatalf("status = %q error = %q", session.Status, session.Error)
	}
}

// 发布出去的会话消息不得共享内部字段。
func TestSessionMessageDoesNotShareInternals(t *testing.T) {
	resetSessions(t)

	identity := sessionIdentity{id: "claude:copy-1", client: SessionClientClaudeCode}
	registerSessionRequest(identity, "claude-opus-5", 1)
	recordSessionSuccess(identity.id, &llm.Usage{PromptTokens: 5, PromptTokensDetails: promptDetailsForTest(2)}, statsMetricsForTest(0))

	sessionMu.Lock()
	message := sessionMessageLocked(sessions[identity.id])
	sessions[identity.id].Usage.PromptTokensDetails.CachedTokens = 99
	sessionMu.Unlock()

	if message.waiters != nil || message.requestIDs != nil {
		t.Fatal("message leaked internal fields")
	}
	if message.Usage.PromptTokensDetails.CachedTokens != 2 {
		t.Fatalf("cached tokens = %d, want 2", message.Usage.PromptTokensDetails.CachedTokens)
	}
}

// Claude Code 会把提醒块注入用户消息，标签必须取用户真实输入而不是这段固定文本。
func TestSessionLabelSkipsInjectedBlocks(t *testing.T) {
	body := []byte(`{"system":"You are Claude Code","messages":[{"role":"user","content":[
		{"type":"text","text":"<system-reminder>\nAs you answer the user's questions, you can use the following context.\n</system-reminder>"},
		{"type":"text","text":"帮我看下这个报错"}]}]}`)
	if label := sessionLabel(SessionClientClaudeCode, body); label != "帮我看下这个报错" {
		t.Fatalf("label = %q", label)
	}
}

// 注入块在每个新会话里完全相同，剔除后不同会话必须派生出不同的内容哈希，否则会共用上一个会话的绑定。
func TestContentSessionIDDistinguishesNewConversations(t *testing.T) {
	reminder := `{"type":"text","text":"<system-reminder>\nAs you answer the user's questions, you can use the following context.\n</system-reminder>"}`
	first := contentSessionID([]byte(`{"system":"You are Claude Code","messages":[{"role":"user","content":[` +
		reminder + `,{"type":"text","text":"第一个会话的问题"}]}]}`))
	second := contentSessionID([]byte(`{"system":"You are Claude Code","messages":[{"role":"user","content":[` +
		reminder + `,{"type":"text","text":"第二个会话的问题"}]}]}`))

	if first == "" || second == "" {
		t.Fatalf("ids = (%q, %q), both should be derived", first, second)
	}
	if first == second {
		t.Fatal("two different conversations collapsed into one session id")
	}
}

// 只有注入块而没有真实用户输入时不得派生标识，否则全部会话会折叠成同一个并共用绑定。
func TestContentSessionIDRefusesWithoutRealUserInput(t *testing.T) {
	id := contentSessionID([]byte(`{"system":"You are Claude Code","messages":[{"role":"user","content":[
		{"type":"text","text":"<system-reminder>\nAs you answer the user's questions.\n</system-reminder>"}]}]}`))
	if id != "" {
		t.Fatalf("id = %q, want empty", id)
	}
}

// metadata.user_id 的驼峰命名与非十六进制后缀都要能取出会话标识。
func TestClaudeMetadataSessionIDVariants(t *testing.T) {
	cases := map[string]string{
		`{"metadata":{"user_id":"{\"sessionId\":\"S-42\"}"}}`:           "S-42",
		`{"metadata":{"user_id":"user_ab_account_cd_session_AbC-123"}}`: "AbC-123",
		`{"metadata":{"user_id":"{\"session_id\":\"2f1e8c04-aaaa\"}"}}`: "2f1e8c04-aaaa",
	}
	for body, want := range cases {
		if got := claudeMetadataSessionID([]byte(body)); got != want {
			t.Fatalf("body %s -> %q, want %q", body, got, want)
		}
	}
}

// bindForTest 直接在会话状态上建立绑定，避开对渠道缓存的依赖。
func bindForTest(sessionID string, channelID int, modelName string) error {
	sessionMu.Lock()
	defer sessionMu.Unlock()

	session := resolveSessionLocked(sessionID)
	if session == nil {
		return errors.New("session not found")
	}
	session.ChannelID = channelID
	session.ChannelName = "channel"
	session.ModelName = modelName
	session.Status = SessionStatusBound
	for waiter := range session.waiters {
		close(waiter)
		delete(session.waiters, waiter)
	}
	return nil
}
