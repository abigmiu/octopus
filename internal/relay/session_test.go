package relay

import (
	"context"
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

// 内容哈希兜底时，同一会话的首轮与后续轮必须落到同一个会话上。
func TestIdentifySessionContentHashAcrossTurns(t *testing.T) {
	resetSessions(t)

	first := identifySession(http.Header{}, []byte(`{"messages":[
		{"role":"system","content":"you are a helper"},
		{"role":"user","content":"first question"}]}`))
	if first.id == "" {
		t.Fatal("first turn produced no session id")
	}
	if first.alias != "" {
		t.Fatalf("first turn alias = %q, want empty", first.alias)
	}

	second := identifySession(http.Header{}, []byte(`{"messages":[
		{"role":"system","content":"you are a helper"},
		{"role":"user","content":"first question"},
		{"role":"assistant","content":"an answer"},
		{"role":"user","content":"second question"}]}`))
	if second.alias != first.id {
		t.Fatalf("second turn alias = %q, want %q", second.alias, first.id)
	}
	if second.id == first.id {
		t.Fatal("second turn primary id should include the assistant message")
	}

	// 首轮登记会话后，后续轮通过备用标识命中同一会话。
	registerSessionRequest(first, "group-a", 1, true)
	registerSessionRequest(second, "group-a", 2, true)

	sessionMu.Lock()
	defer sessionMu.Unlock()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	if got := sessions[first.id].RequestCount; got != 2 {
		t.Fatalf("request count = %d, want 2", got)
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
	registerSessionRequest(identity, "group-a", 1, true)

	type outcome struct {
		target sessionTarget
		ok     bool
	}
	done := make(chan outcome, 1)
	go func() {
		target, ok, _ := awaitSessionTarget(context.Background(), identity.id)
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
	registerSessionRequest(identity, "group-a", 1, true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := awaitSessionTarget(ctx, identity.id)
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
	registerSessionRequest(pending, "group-a", 1, true)
	sessionMu.Lock()
	sessions[pending.id].PendingCount = 1
	sessions[pending.id].LastActiveAt = time.Now().Add(-time.Hour)
	sessionMu.Unlock()

	for i := 0; i < maxSessions+3; i++ {
		identity := sessionIdentity{id: "claude:bulk-" + string(rune('a'+i)), client: SessionClientClaudeCode}
		registerSessionRequest(identity, "group-a", uint64(100+i), true)
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
	registerSessionRequest(identity, "group-a", 1, true)
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
	registerSessionRequest(identity, "group-a", 1, true)
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
