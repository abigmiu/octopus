package relay

import (
	"context"
	"fmt"
	"hash/fnv"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/looplj/axonhub/llm"
	"github.com/tidwall/gjson"
)

// 会话在等待人工选择上游目标时的状态。
type SessionStatus string

const (
	SessionStatusPending SessionStatus = "pending" // 尚未绑定上游目标, 该会话的请求正在等待人工选择。
	SessionStatusBound   SessionStatus = "bound"   // 已绑定上游目标, 后续请求直接沿用。
	SessionStatusError   SessionStatus = "error"   // 已绑定但最近一次请求失败, 需要人工换目标或等待上游恢复。
)

const (
	sessionSelectTimeout  = 60 * time.Second // 单个请求等待人工选择目标的上限, 超时后该请求以明确错误失败。
	sessionPendingLinger  = 5 * time.Minute  // 选择超时后待选条目继续保留的时长, 使客户端重试能命中随后的人工选择。
	sessionStreamBuffer   = 16               // 单个会话流连接的非阻塞消息缓冲容量。
	maxSessions           = 20               // 进程内最多保留的会话数量。
	maxRequestsPerSession = 200              // 单个会话最多保留的请求数量。
	sessionLabelRuneLimit = 40               // 可读标签取用首条用户消息的字符数上限。
	sessionHashTextLimit  = 100              // 内容哈希取用单段文本的字符数上限。
)

// 客户端类型, 用于会话标签和前端展示。
const (
	SessionClientClaudeCode = "claude-code"
	SessionClientCodex      = "codex"
	SessionClientOpenAISDK  = "openai-sdk"
	SessionClientUnknown    = "unknown"
)

// SessionState 是一个客户端会话的进程内状态, 同时作为会话流的消息形状。
type SessionState struct {
	ID             string        `json:"id"`              // 会话标识, 由客户端信号或请求内容派生。
	Label          string        `json:"label"`           // 可读标签。
	Client         string        `json:"client"`          // 客户端类型。
	RequestedModel string        `json:"requested_model"` // 客户端最近一次请求的模型名称, 仅用于展示, 不参与选路。
	Status         SessionStatus `json:"status"`          // 会话当前状态。
	ChannelID      int           `json:"channel_id"`      // 已绑定的上游渠道 ID, 0 表示尚未绑定。
	ChannelName    string        `json:"channel_name"`    // 已绑定渠道的名称。
	ModelName      string        `json:"model_name"`      // 已绑定的上游模型名称。

	StartedAt    time.Time `json:"started_at"`     // 会话首次出现时间。
	LastActiveAt time.Time `json:"last_active_at"` // 会话最近一次请求时间。
	RequestCount int       `json:"request_count"`  // 该会话累计请求数。
	PendingCount int       `json:"pending_count"`  // 当前正在等待人工选择的请求数。

	Usage llm.Usage `json:"usage"`           // 该会话累计用量。
	Cost  float64   `json:"cost"`            // 该会话累计费用。
	Error string    `json:"error,omitempty"` // 最近一次失败原因。

	// waiters 是当前阻塞等待绑定的请求的唤醒通道; 绑定建立或会话被清理时全部关闭。
	waiters map[chan struct{}]struct{}
	// pendingUntil 是选择超时后待选条目的保留截止时间, 零值表示不处于超时保留状态。
	pendingUntil time.Time
	// requestIDs 保存该会话的请求 ID, 按登记顺序排列, 用于按会话裁剪历史。
	requestIDs []uint64
}

var (
	sessionMu      sync.Mutex                             // sessionMu 保护全部会话状态。
	sessions       = make(map[string]*SessionState)       // sessions 按规范会话标识保存状态。
	sessionAliases = make(map[string]string)              // sessionAliases 把备用标识指向规范标识, 使同一会话的多种标识命中同一条状态。
	sessionStreams = make(map[chan SessionState]struct{}) // 全部会话 SSE 连接。
)

// legacyClaudeSessionPattern 匹配旧版 Claude Code 写在 metadata.user_id 末尾的会话标识。
// 标识不限于十六进制: 不同版本可能写入任意非下划线字符组成的值。
var legacyClaudeSessionPattern = regexp.MustCompile(`_session_([^_]+)$`)

// sessionIdentity 是一次请求解析出的会话身份。
type sessionIdentity struct {
	id     string // 主会话标识。
	alias  string // 备用会话标识, 内容哈希兜底时用于让首轮与后续轮命中同一会话, 可为空。
	label  string // 可读标签。
	client string // 客户端类型。
}

// identifySession 解析一次请求的会话身份; 优先使用客户端显式信号, 否则由请求内容派生稳定哈希。
func identifySession(headers http.Header, body []byte) sessionIdentity {
	identity := sessionIdentity{client: detectClient(headers, body)}
	identity.label = sessionLabel(identity.client, body)

	if id, alias := explicitSessionID(headers, body); id != "" {
		identity.id = id
		identity.alias = alias
		return identity
	}
	// 客户端没有任何会话信号时退回内容哈希: 同一会话的系统提示与首条用户消息始终不变, 因此哈希稳定。
	identity.id = contentSessionID(body)
	return identity
}

// explicitSessionID 按优先级取出客户端显式携带的会话标识, 返回主标识与备用标识。
func explicitSessionID(headers http.Header, body []byte) (string, string) {
	if id := normalizeSessionID(headerValue(headers, "X-Claude-Code-Session-Id")); id != "" {
		return "claude:" + id, ""
	}
	if id := claudeMetadataSessionID(body); id != "" {
		return "claude:" + id, ""
	}
	for _, name := range []string{"Session-Id", "Session_id"} {
		if id := normalizeSessionID(headerValue(headers, name)); id != "" {
			return "codex:" + id, ""
		}
	}
	if id := normalizeSessionID(headerValue(headers, "X-Session-Id")); id != "" {
		return "header:" + id, ""
	}
	if id := normalizeSessionID(headerValue(headers, "X-Session-Affinity")); id != "" {
		return "affinity:" + id, ""
	}
	if id := normalizeSessionID(headerValue(headers, "X-Client-Request-Id")); id != "" {
		return "clientreq:" + id, ""
	}
	if len(body) == 0 {
		return "", ""
	}

	for _, path := range []string{"session_id", "sessionId"} {
		if id := normalizeSessionID(gjson.GetBytes(body, path).String()); id != "" {
			return "session:" + id, ""
		}
	}
	// 会话标识可能同时以 prompt_cache_key 和 conversation 出现, 两者互为别名。
	conversationID := ""
	conversation := gjson.GetBytes(body, "conversation")
	if id := normalizeSessionID(conversation.Get("id").String()); id != "" {
		conversationID = "conv:" + id
	} else if conversation.Type == gjson.String {
		if id := normalizeSessionID(conversation.String()); id != "" {
			conversationID = "conv:" + id
		}
	}
	if id := normalizeSessionID(gjson.GetBytes(body, "prompt_cache_key").String()); id != "" {
		return "pck:" + id, conversationID
	}
	if conversationID != "" {
		return conversationID, ""
	}
	if id := normalizeSessionID(gjson.GetBytes(body, "metadata.user_id").String()); id != "" {
		return "user:" + id, ""
	}
	if id := normalizeSessionID(gjson.GetBytes(body, "conversation_id").String()); id != "" {
		return "conv:" + id, ""
	}
	return "", ""
}

// claudeMetadataSessionID 从 Claude Code 写入的 metadata.user_id 中取出会话标识, 兼容 JSON 与旧版后缀两种格式。
func claudeMetadataSessionID(body []byte) string {
	userID := strings.TrimSpace(gjson.GetBytes(body, "metadata.user_id").String())
	if userID == "" {
		return ""
	}
	if strings.HasPrefix(userID, "{") {
		// 不同版本分别使用下划线与驼峰命名。
		for _, path := range []string{"session_id", "sessionId"} {
			if id := normalizeSessionID(gjson.Get(userID, path).String()); id != "" {
				return id
			}
		}
		return ""
	}
	if matches := legacyClaudeSessionPattern.FindStringSubmatch(userID); len(matches) >= 2 {
		return normalizeSessionID(matches[1])
	}
	return ""
}

// contentSessionID 由系统提示和首条用户消息派生会话标识。
// 这两段内容在整个会话里从不变化, 因此标识从首轮到末轮保持一致, 不需要在轮次之间切换标识。
// 首条助手消息不参与派生: 它只能区分"开头相同但后续分叉"的会话, 而那时绑定早已在本标识下建立, 收益接近于零,
// 代价却是主标识要在首轮与后续轮之间跳变一次。
// 必须取到真实的用户输入才派生标识: 仅凭系统提示无法区分会话, 各会话会折叠成同一个而错误地共用绑定。
func contentSessionID(body []byte) string {
	system, user, _ := conversationHead(body)
	if user == "" {
		return ""
	}
	return contentHash(system, user)
}

// contentHash 将会话开头的文本折叠成短标识。
func contentHash(system, user string) string {
	digest := fnv.New64a()
	for _, part := range []struct {
		prefix string
		text   string
	}{{"sys:", system}, {"usr:", user}} {
		if part.text != "" {
			digest.Write([]byte(part.prefix + part.text + "\n"))
		}
	}
	return fmt.Sprintf("msg:%016x", digest.Sum64())
}

// conversationHead 取出请求中的系统提示, 首条用户消息和首条助手消息, 覆盖三种客户端协议的正文结构。
func conversationHead(body []byte) (string, string, string) {
	if len(body) == 0 {
		return "", "", ""
	}
	var system, user, assistant string
	assign := func(role, text string) {
		if text == "" {
			return
		}
		switch role {
		case "system", "developer":
			if system == "" {
				system = truncateRunes(text, sessionHashTextLimit)
			}
		case "user":
			if user == "" {
				user = truncateRunes(text, sessionHashTextLimit)
			}
		case "assistant":
			if assistant == "" {
				assistant = truncateRunes(text, sessionHashTextLimit)
			}
		}
	}

	// Anthropic 协议的系统提示位于顶层, 可能是字符串或分段数组。
	if top := gjson.GetBytes(body, "system"); top.Exists() {
		assign("system", messageText(top))
	}
	// OpenAI Chat 与 Anthropic 协议的消息列表。
	if messages := gjson.GetBytes(body, "messages"); messages.IsArray() {
		messages.ForEach(func(_, message gjson.Result) bool {
			assign(message.Get("role").String(), messageText(message.Get("content")))
			return system == "" || user == "" || assistant == ""
		})
	}
	// OpenAI Responses 协议的指令与输入项。
	if instructions := gjson.GetBytes(body, "instructions"); instructions.Exists() {
		assign("system", messageText(instructions))
	}
	if input := gjson.GetBytes(body, "input"); input.Exists() {
		if input.Type == gjson.String {
			assign("user", input.String())
		} else if input.IsArray() {
			input.ForEach(func(_, item gjson.Result) bool {
				// 推理项与函数调用项不属于对话开头, 跳过。
				if itemType := item.Get("type").String(); itemType != "" && itemType != "message" {
					return true
				}
				assign(item.Get("role").String(), messageText(item.Get("content")))
				return system == "" || user == "" || assistant == ""
			})
		}
	}
	return system, user, assistant
}

// messageText 取出一段消息内容中的文本, 兼容字符串, 分段数组和嵌套结构; 客户端注入的上下文块会被剔除。
func messageText(content gjson.Result) string {
	switch {
	case content.Type == gjson.String:
		return cleanMessageText(content.String())
	case content.IsArray():
		texts := make([]string, 0, 4)
		content.ForEach(func(_, part gjson.Result) bool {
			if part.Type == gjson.String {
				if text := cleanMessageText(part.String()); text != "" {
					texts = append(texts, text)
				}
				return true
			}
			switch part.Get("type").String() {
			case "text", "input_text", "output_text", "":
				if text := cleanMessageText(part.Get("text").String()); text != "" {
					texts = append(texts, text)
				}
			}
			return true
		})
		return strings.Join(texts, " ")
	case content.IsObject():
		if text := cleanMessageText(content.Get("text").String()); text != "" {
			return text
		}
		if nested := content.Get("content"); nested.Exists() {
			return messageText(nested)
		}
		if parts := content.Get("parts"); parts.Exists() {
			return messageText(parts)
		}
	}
	return ""
}

// injectedBlockTags 是客户端注入到用户消息中的上下文块标签。
// 这类内容在每个新会话里完全相同, 既不能作为可读标签, 也不能参与会话标识的派生:
// 否则全部新会话会折叠成同一个标识, 从而错误地共用上一个会话的绑定。
var injectedBlockTags = []string{
	"system-reminder",
	"command-name",
	"command-message",
	"command-args",
	"local-command-stdout",
	"local-command-stderr",
	"local-command-caveat",
}

// injectedBlockPattern 匹配上述全部注入块; RE2 不支持反向引用, 故按标签逐一列出完整配对。
var injectedBlockPattern = regexp.MustCompile(func() string {
	alternatives := make([]string, 0, len(injectedBlockTags))
	for _, tag := range injectedBlockTags {
		alternatives = append(alternatives, "<"+tag+">.*?</"+tag+">")
	}
	return "(?is)" + strings.Join(alternatives, "|")
}())

// cleanMessageText 剔除注入的上下文块并压缩空白, 得到用户实际输入的文本。
func cleanMessageText(raw string) string {
	if raw == "" {
		return ""
	}
	cleaned := injectedBlockPattern.ReplaceAllString(raw, " ")
	// 未闭合的注入块同样不能留下, 例如被上游截断的提醒。
	for _, tag := range injectedBlockTags {
		if index := strings.Index(strings.ToLower(cleaned), "<"+tag+">"); index >= 0 {
			cleaned = cleaned[:index]
		}
	}
	return strings.TrimSpace(strings.Join(strings.Fields(cleaned), " "))
}

// detectClient 按客户端请求头和正文特征识别客户端类型。
func detectClient(headers http.Header, body []byte) string {
	agent := strings.ToLower(headerValue(headers, "User-Agent"))
	switch {
	case headerValue(headers, "X-Claude-Code-Session-Id") != "" || strings.Contains(agent, "claude-cli") || strings.Contains(agent, "claude-code"):
		return SessionClientClaudeCode
	case headerValue(headers, "Session-Id") != "" || headerValue(headers, "Session_id") != "" || strings.Contains(agent, "codex"):
		return SessionClientCodex
	}
	if len(body) > 0 && claudeMetadataSessionID(body) != "" {
		return SessionClientClaudeCode
	}
	if strings.Contains(agent, "openai") || headerValue(headers, "X-Stainless-Lang") != "" {
		return SessionClientOpenAISDK
	}
	return SessionClientUnknown
}

// sessionLabel 生成会话的可读标签: 首条用户消息的开头, 取不到时退回客户端类型。
func sessionLabel(client string, body []byte) string {
	_, user, _ := conversationHead(body)
	if user = strings.TrimSpace(strings.Join(strings.Fields(user), " ")); user != "" {
		return truncateRunes(user, sessionLabelRuneLimit)
	}
	return client
}

// headerValue 不区分大小写地取出首个非空请求头值。
func headerValue(headers http.Header, name string) string {
	if headers == nil {
		return ""
	}
	return strings.TrimSpace(headers.Get(name))
}

// normalizeSessionID 校验客户端提供的会话标识, 拒绝空值, 超长值和含控制字符的值。
func normalizeSessionID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 256 {
		return ""
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return ""
		}
	}
	return raw
}

// truncateRunes 按字符数截断文本, 避免从多字节字符中间切开。
func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

// sessionTarget 是一个会话绑定的上游目标。
type sessionTarget struct {
	channelID int
	modelName string
}

// resolveSessionLocked 按主标识或备用标识取出会话; 调用方必须持有锁。
func resolveSessionLocked(id string) *SessionState {
	if session := sessions[id]; session != nil {
		return session
	}
	if canonical, ok := sessionAliases[id]; ok {
		return sessions[canonical]
	}
	return nil
}

// linkSessionLocked 把一个备用标识指向已有会话, 使该会话的多种标识都能命中同一条状态; 调用方必须持有锁。
// 会话对象只在 sessions 中占用规范标识一个键, 备用标识一律走别名表, 由此保证淘汰时不会留下悬空引用。
func linkSessionLocked(alias string, session *SessionState) {
	if alias == "" || session == nil || alias == session.ID {
		return
	}
	sessionAliases[alias] = session.ID
}

// dropSessionLocked 删除会话及其全部备用标识; 调用方必须持有锁。
func dropSessionLocked(id string) {
	delete(sessions, id)
	for alias, canonical := range sessionAliases {
		if canonical == id {
			delete(sessionAliases, alias)
		}
	}
}

// sessionForLocked 取出会话, 不存在时按待选状态新建; 调用方必须持有锁。
// 会话可能因人工清空日志, 数量超限被淘汰或备用标识变化而消失, 这些情况都不该让客户端请求失败,
// 只该让它重新等待一次人工选择。
func sessionForLocked(identity sessionIdentity) *SessionState {
	if session := resolveSessionLocked(identity.id); session != nil {
		// 主标识首次出现时补上别名, 使后续只携带其中一种标识的请求也能命中。
		if sessions[identity.id] == nil {
			linkSessionLocked(identity.id, session)
		}
		linkSessionLocked(identity.alias, session)
		return session
	}
	if identity.alias != "" {
		if session := resolveSessionLocked(identity.alias); session != nil {
			linkSessionLocked(identity.id, session)
			return session
		}
	}
	session := &SessionState{
		ID:        identity.id,
		Label:     identity.label,
		Client:    identity.client,
		Status:    SessionStatusPending,
		StartedAt: time.Now(),
		waiters:   make(map[chan struct{}]struct{}),
	}
	sessions[identity.id] = session
	linkSessionLocked(identity.alias, session)
	return session
}

// registerSessionRequest 将一次请求登记到其会话, 会话不存在时创建; 返回该会话已绑定的目标。
// 新会话一律以待选状态出现: 每个会话都必须由人工选择一次渠道和模型, 请求在此之前不会发往上游。
func registerSessionRequest(identity sessionIdentity, requestedModel string, requestID uint64) sessionTarget {
	sessionMu.Lock()
	defer sessionMu.Unlock()

	session := sessionForLocked(identity)
	session.RequestedModel = requestedModel
	session.LastActiveAt = time.Now()
	session.RequestCount++
	session.requestIDs = append(session.requestIDs, requestID)
	if len(session.requestIDs) > maxRequestsPerSession {
		dropped := session.requestIDs[0]
		session.requestIDs = session.requestIDs[1:]
		dropRequest(dropped)
	}
	publishSessionLocked(session)
	trimSessionsLocked()
	return sessionTarget{channelID: session.ChannelID, modelName: session.ModelName}
}

// awaitSessionTarget 阻塞等待会话绑定上游目标, 期间该会话在前端显示为待选。
// 返回绑定成功的目标; 客户端断开或等待超时时返回 ok 为 false, err 说明原因。
// 会话状态在等待期间可能被人工清空或淘汰, 此时按待选重新建立而不是让请求失败: 丢失的只是记账, 请求应当重新等待选择。
func awaitSessionTarget(ctx context.Context, identity sessionIdentity) (sessionTarget, bool, error) {
	sessionMu.Lock()
	session := sessionForLocked(identity)
	// 已经绑定时无需等待, 直接沿用。
	if session.ChannelID != 0 {
		target := sessionTarget{channelID: session.ChannelID, modelName: session.ModelName}
		sessionMu.Unlock()
		return target, true, nil
	}
	waiter := make(chan struct{})
	session.waiters[waiter] = struct{}{}
	session.PendingCount++
	session.Status = SessionStatusPending
	session.pendingUntil = time.Time{}
	publishSessionLocked(session)
	sessionMu.Unlock()

	timer := time.NewTimer(sessionSelectTimeout)
	defer timer.Stop()

	var waitErr error
	timedOut := false
	select {
	case <-waiter:
		// 绑定已建立或会话已被清理, 由下面重新读取状态判断。
	case <-ctx.Done():
		waitErr = ctx.Err()
	case <-timer.C:
		timedOut = true
		waitErr = fmt.Errorf("no channel selected for this session within %s", sessionSelectTimeout)
	}

	sessionMu.Lock()
	defer sessionMu.Unlock()

	// 等待期间会话可能已被替换, 重新解析一次并归还本次的待选计数。
	current := resolveSessionLocked(identity.id)
	if current == nil {
		current = session
	}
	delete(current.waiters, waiter)
	delete(session.waiters, waiter)
	if current.PendingCount > 0 {
		current.PendingCount--
	}
	// 选择超时后继续保留待选条目, 使随后的人工选择能被客户端重试立即命中。
	if timedOut {
		current.pendingUntil = time.Now().Add(sessionPendingLinger)
	}
	if current.ChannelID != 0 {
		publishSessionLocked(current)
		return sessionTarget{channelID: current.ChannelID, modelName: current.ModelName}, true, nil
	}
	publishSessionLocked(current)
	return sessionTarget{}, false, waitErr
}

// SessionBind 为一个会话选定上游目标; 已在等待的请求立即继续, 正在进行的上游调用被中止以便改道。
func SessionBind(sessionID string, channelID int, modelName string) error {
	channel, err := op.ChannelGet(channelID)
	if err != nil {
		return err
	}
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return fmt.Errorf("model name is required")
	}

	sessionMu.Lock()
	session := resolveSessionLocked(sessionID)
	if session == nil {
		sessionMu.Unlock()
		return fmt.Errorf("session not found")
	}
	changed := session.ChannelID != channelID || session.ModelName != modelName
	session.ChannelID = channelID
	session.ChannelName = channel.Name
	session.ModelName = modelName
	session.Status = SessionStatusBound
	session.Error = ""
	session.pendingUntil = time.Time{}
	// 唤醒全部等待中的请求, 它们随后重新读取绑定并继续。
	for waiter := range session.waiters {
		close(waiter)
		delete(session.waiters, waiter)
	}
	requestIDs := make([]uint64, len(session.requestIDs))
	copy(requestIDs, session.requestIDs)
	publishSessionLocked(session)
	sessionMu.Unlock()

	// 目标变更时中止该会话仍在等待上游响应的请求, 使它们立即改用新目标而不必等到本轮结束。
	if changed {
		interruptSending(requestIDs)
	}
	return nil
}

// SessionUnbind 解除一个会话的绑定, 该会话的下一条请求重新等待人工选择。
func SessionUnbind(sessionID string) error {
	sessionMu.Lock()
	session := resolveSessionLocked(sessionID)
	if session == nil {
		sessionMu.Unlock()
		return fmt.Errorf("session not found")
	}
	session.ChannelID = 0
	session.ChannelName = ""
	session.ModelName = ""
	session.Status = SessionStatusPending
	session.pendingUntil = time.Time{}
	requestIDs := make([]uint64, len(session.requestIDs))
	copy(requestIDs, session.requestIDs)
	publishSessionLocked(session)
	sessionMu.Unlock()

	// 中止仍在进行的上游调用, 使这些请求回到等待选择状态而不是继续用旧目标。
	interruptSending(requestIDs)
	return nil
}

// recordSessionFailure 记录会话最近一次失败原因并标记为异常状态, 绑定保持不变以便人工换目标。
func recordSessionFailure(sessionID, reason string) {
	if sessionID == "" || reason == "" {
		return
	}
	sessionMu.Lock()
	defer sessionMu.Unlock()

	session := resolveSessionLocked(sessionID)
	if session == nil {
		return
	}
	session.Error = reason
	// 未绑定的会话仍在等待选择, 失败原因不改变其待选状态。
	if session.ChannelID != 0 {
		session.Status = SessionStatusError
	}
	publishSessionLocked(session)
}

// recordSessionSuccess 累加会话用量与费用并清除异常状态。
func recordSessionSuccess(sessionID string, usage *llm.Usage, metrics model.StatsMetrics) {
	if sessionID == "" {
		return
	}
	sessionMu.Lock()
	defer sessionMu.Unlock()

	session := resolveSessionLocked(sessionID)
	if session == nil {
		return
	}
	if usage != nil {
		session.Usage.PromptTokens += usage.PromptTokens
		session.Usage.CompletionTokens += usage.CompletionTokens
		session.Usage.TotalTokens += usage.TotalTokens
		if usage.PromptTokensDetails != nil {
			if session.Usage.PromptTokensDetails == nil {
				session.Usage.PromptTokensDetails = &llm.PromptTokensDetails{}
			}
			session.Usage.PromptTokensDetails.CachedTokens += usage.PromptTokensDetails.CachedTokens
			session.Usage.PromptTokensDetails.WriteCachedTokens += usage.PromptTokensDetails.WriteCachedTokens
		}
	}
	session.Cost += metrics.InputCost + metrics.OutputCost
	session.Error = ""
	if session.ChannelID != 0 {
		session.Status = SessionStatusBound
	}
	publishSessionLocked(session)
}

// trimSessionsLocked 在会话数超出上限时淘汰最旧的空闲会话; 调用方必须持有锁。
func trimSessionsLocked() {
	if len(sessions) <= maxSessions {
		return
	}
	now := time.Now()
	type candidate struct {
		id           string
		lastActiveAt time.Time
	}
	candidates := make([]candidate, 0, len(sessions))
	for id, session := range sessions {
		// 仍有请求在等待选择, 或处于超时保留期内的会话不能淘汰, 否则人工选择将失去目标。
		if len(session.waiters) > 0 || session.PendingCount > 0 || session.pendingUntil.After(now) {
			continue
		}
		candidates = append(candidates, candidate{id: id, lastActiveAt: session.LastActiveAt})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].lastActiveAt.Before(candidates[j].lastActiveAt) })
	for _, item := range candidates {
		if len(sessions) <= maxSessions {
			return
		}
		if session := sessions[item.id]; session != nil {
			for _, requestID := range session.requestIDs {
				dropRequest(requestID)
			}
		}
		dropSessionLocked(item.id)
	}
}

// SessionList 返回全部会话状态, 按最近活跃时间倒序。
func SessionList() []SessionState {
	sessionMu.Lock()
	defer sessionMu.Unlock()

	list := make([]SessionState, 0, len(sessions))
	for _, session := range sessions {
		list = append(list, sessionMessageLocked(session))
	}
	sort.Slice(list, func(i, j int) bool { return list[i].LastActiveAt.After(list[j].LastActiveAt) })
	return list
}

// SessionClear 删除全部没有等待中请求的会话。
func SessionClear() {
	sessionMu.Lock()
	defer sessionMu.Unlock()

	now := time.Now()
	for id, session := range sessions {
		if len(session.waiters) > 0 || session.PendingCount > 0 || session.pendingUntil.After(now) {
			continue
		}
		dropSessionLocked(id)
	}
}

// sessionMessageLocked 复制会话状态用于发布, 内部字段不外泄; 调用方必须持有锁。
func sessionMessageLocked(session *SessionState) SessionState {
	message := *session
	message.waiters = nil
	message.requestIDs = nil
	if session.Usage.PromptTokensDetails != nil {
		details := *session.Usage.PromptTokensDetails
		message.Usage.PromptTokensDetails = &details
	}
	return message
}

// publishSessionLocked 非阻塞发布会话状态, 连接拥塞时关闭它并交给客户端重连获取全量快照; 调用方必须持有锁。
func publishSessionLocked(session *SessionState) {
	message := sessionMessageLocked(session)
	for stream := range sessionStreams {
		select {
		case stream <- message:
		default:
			delete(sessionStreams, stream)
			close(stream)
		}
	}
}

// OpenSessionStream 注册会话流连接, 返回按最近活跃时间倒序的全部快照和后续增量通道。
func OpenSessionStream() ([]SessionState, chan SessionState) {
	sessionMu.Lock()
	defer sessionMu.Unlock()

	stream := make(chan SessionState, sessionStreamBuffer)
	sessionStreams[stream] = struct{}{}

	snapshot := make([]SessionState, 0, len(sessions))
	for _, session := range sessions {
		snapshot = append(snapshot, sessionMessageLocked(session))
	}
	sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].LastActiveAt.After(snapshot[j].LastActiveAt) })
	return snapshot, stream
}

// CloseSessionStream 注销并关闭指定会话流连接。
func CloseSessionStream(stream chan SessionState) {
	sessionMu.Lock()
	defer sessionMu.Unlock()

	if _, exists := sessionStreams[stream]; exists {
		delete(sessionStreams, stream)
		close(stream)
	}
}
