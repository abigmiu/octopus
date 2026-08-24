package relay

import (
	"context"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/looplj/axonhub/llm"
)

// 客户端请求在转发过程中的当前状态。
type Status string

const (
	StatusRunning   Status = "running"   // 循环中: 正在选目标, 等待或请求上游。
	StatusCommitted Status = "committed" // 首字节已写出客户端, 此后不可再重试。
	StatusSuccess   Status = "success"   // 响应已完整交付客户端。
	StatusFailed    Status = "failed"    // 请求以错误结束。
	StatusCanceled  Status = "canceled"  // 客户端提前断开或取消。
)

// 请求被取消的具体原因, 区分自不同来源以免统一显示为 context canceled。
const (
	clientCanceledReason = "client disconnected"                        // 客户端在等待或选路期间提前断开。
	writeCanceledReason  = "client disconnected while writing response" // 响应写出过程中客户端断开。
)

// RoundRecord 是一轮上游调用的完整结果, 请求结束后依然保留以便回溯每次尝试的失败原因。
type RoundRecord struct {
	Round      int       `json:"round"`           // 轮次序号。
	Channel    string    `json:"channel"`         // 本轮选中的渠道名称。
	Model      string    `json:"model"`           // 本轮实际请求上游的模型名称。
	StartedAt  time.Time `json:"started_at"`      // 本轮开始时间。
	DurationMS int64     `json:"duration_ms"`     // 本轮耗时毫秒, 未结束时为零。
	Error      string    `json:"error,omitempty"` // 本轮失败原因, 为空表示取得了可提交响应。
}

// 客户端请求的完整进程内状态, 同时作为状态流的消息形状; 上半部分在请求到达时写入并在结束时定稿, 下半部分每轮循环覆盖。
type RequestState struct {
	ID        uint64        `json:"id"`         // 请求在当前进程内的唯一标识。
	Status    Status        `json:"status"`     // 请求当前状态。
	StartedAt time.Time     `json:"started_at"` // 请求到达时间。
	Duration  time.Duration `json:"duration"`   // 请求总耗时, 未结束时为零。
	Model     string        `json:"model"`      // 客户端请求的模型名称, 即分组名称。
	Usage     llm.Usage     `json:"usage"`      // 请求结束时写入的展示用量。
	Cost      float64       `json:"cost"`       // 请求结束时写入的累计费用。

	SessionID    string `json:"session_id"`    // 该请求所属的客户端会话标识。
	SessionLabel string `json:"session_label"` // 会话的可读标签。

	Round         int    `json:"round"`                   // 最新一轮循环的递增序号, 人工中止按此匹配以免误杀下一轮。
	TargetChannel string `json:"target_channel"`          // 最新一轮选中的渠道名称。
	TargetModel   string `json:"target_model"`            // 最新一轮实际请求上游的模型名称。
	Sending       bool   `json:"sending"`                 // 最新一轮是否仍在等待上游响应。
	Error         string `json:"error,omitempty"`         // 最后一次真实失败原因, 请求被取消时不会被取消原因覆盖。
	CancelReason  string `json:"cancel_reason,omitempty"` // 请求被取消的原因, 与失败原因分开记录。

	Rounds []RoundRecord `json:"rounds,omitempty"` // 全部轮次的结果, 按轮次升序。

	body         string             // 客户端原始请求体, 体积大故不进状态流, 由独立接口按需拉取。
	responseBody string             // 聚合后的完整最终响应体, 同样按需拉取。
	apiKeyID     int                // 发起请求的 API Key ID, 用于请求完成后的归属统计。
	cancel       context.CancelFunc // 中止最新一轮上游请求, 仅在该轮等待响应期间非空。
	roundStarted time.Time          // 最新一轮的开始时间, 用于结束时计算耗时。
}

const streamBuffer = 16 // 单个状态流连接的非阻塞消息缓冲容量。

var (
	idSeq    atomic.Uint64                          // 进程内严格递增的请求 ID。
	mu       sync.Mutex                             // 全部共享状态的互斥锁。
	requests = make(map[uint64]*RequestState)       // 按请求 ID 保存的全部请求状态。
	watchers = make(map[chan RequestState]struct{}) // 全部状态流 SSE 连接。
)

// newRequestState 分配请求 ID 并登记初始运行状态; 返回的记录是本请求后续全部状态写入的入口。
func newRequestState(model, body string, apiKeyID int) *RequestState {
	mu.Lock()
	defer mu.Unlock()

	request := &RequestState{
		ID:        idSeq.Add(1),
		Status:    StatusRunning,
		StartedAt: time.Now(),
		Model:     model,
		body:      body,
		apiKeyID:  apiKeyID,
	}
	requests[request.ID] = request
	publishRequestLocked(request)
	return request
}

// setSession 记录该请求所属的会话, 使前端可以把请求归入会话分组。
func (r *RequestState) setSession(id, label string) {
	mu.Lock()
	defer mu.Unlock()

	r.SessionID = id
	r.SessionLabel = label
	publishRequestLocked(r)
}

// startRound 记录本轮选中的目标并进入上游请求, cancel 供人工中止本轮, 返回递增的轮次序号。
func (r *RequestState) startRound(cancel context.CancelFunc, channel, model string) int {
	mu.Lock()
	defer mu.Unlock()

	r.Round++
	r.TargetChannel = channel
	r.TargetModel = model
	r.Sending = true
	r.cancel = cancel
	r.roundStarted = time.Now()
	r.Rounds = append(r.Rounds, RoundRecord{Round: r.Round, Channel: channel, Model: model, StartedAt: r.roundStarted})
	publishRequestLocked(r)
	return r.Round
}

// finishRound 记录本轮上游结果, errText 为空表示已取得可提交响应。
// clientGone 表示本轮结束是因为客户端自己放弃: 此时上游错误只是上下文取消的转述, 不作为请求的失败原因,
// 否则此前各轮真实的上游错误会被 context canceled 一类的信息覆盖。
func (r *RequestState) finishRound(errText string, clientGone bool) {
	mu.Lock()
	defer mu.Unlock()

	r.Sending = false
	r.cancel = nil
	if clientGone {
		errText = clientCanceledReason
	} else if errText != "" {
		r.Error = errText
	}
	if count := len(r.Rounds); count > 0 && r.Rounds[count-1].Round == r.Round {
		r.Rounds[count-1].Error = errText
		r.Rounds[count-1].DurationMS = time.Since(r.Rounds[count-1].StartedAt).Milliseconds()
	}
	publishRequestLocked(r)
}

// Interrupt 中止指定请求仍在等待响应且轮次匹配的上游请求; 轮次不匹配说明该轮已结束, 不影响后续轮次。
func Interrupt(id uint64, round int) {
	mu.Lock()
	request := requests[id]
	if request == nil || request.Round != round || request.cancel == nil {
		mu.Unlock()
		return
	}
	cancel := request.cancel
	request.cancel = nil
	mu.Unlock()

	cancel()
}

// interruptSending 中止给定请求中仍在等待上游响应的那些轮次, 用于会话目标变更后立即改道。
func interruptSending(ids []uint64) {
	mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(ids))
	for _, id := range ids {
		request := requests[id]
		if request == nil || !request.Sending || request.cancel == nil {
			continue
		}
		cancels = append(cancels, request.cancel)
		request.cancel = nil
	}
	mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
}

// dropRequest 删除一条已结束的请求记录, 由会话侧按会话容量上限调用; 仍在进行的请求不会被删除。
func dropRequest(id uint64) {
	mu.Lock()
	defer mu.Unlock()

	if request := requests[id]; request != nil && request.Status != StatusRunning && request.Status != StatusCommitted {
		delete(requests, id)
	}
}

// wait 在重新选择目标之前退避 seconds 秒; 客户端在退避期间断开时以取消终态定稿并返回 false。
func (r *RequestState) wait(ctx context.Context, seconds int) bool {
	select {
	case <-ctx.Done():
		r.markCanceled(clientCanceledReason, "", nil)
		return false
	case <-time.After(time.Duration(seconds) * time.Second):
		return true
	}
}

// markCommitted 标记响应已提交; 流式响应在此之后仍会持续转发, 故必须先于提交动作调用。
func (r *RequestState) markCommitted() {
	mu.Lock()
	defer mu.Unlock()

	r.Status = StatusCommitted
	publishRequestLocked(r)
}

// markSucceeded 以成功终态定稿请求。
func (r *RequestState) markSucceeded(responseBody string, usage *llm.Usage) {
	mu.Lock()
	defer mu.Unlock()

	r.Status = StatusSuccess
	r.Error = ""
	r.CancelReason = ""
	r.responseBody = responseBody
	r.finishLocked(usage)
}

// markFailed 以失败终态定稿请求, 最终错误取自本次失败原因。
func (r *RequestState) markFailed(err error, responseBody string, usage *llm.Usage) {
	mu.Lock()
	defer mu.Unlock()

	r.Status = StatusFailed
	r.Error = err.Error()
	if responseBody != "" {
		r.responseBody = responseBody
	}
	r.finishLocked(usage)
}

// markCanceled 以取消终态定稿请求, 用于客户端提前断开或主动取消。
// 取消原因单独记录, 不覆盖此前各轮的真实失败原因, 否则重试过程中的上游错误会全部丢失。
func (r *RequestState) markCanceled(reason string, responseBody string, usage *llm.Usage) {
	mu.Lock()
	defer mu.Unlock()

	r.Status = StatusCanceled
	r.CancelReason = reason
	if responseBody != "" {
		r.responseBody = responseBody
	}
	r.finishLocked(usage)
}

// finishLocked 写入用量和费用, 发布终态并更新请求级统计; 调用方必须持有锁。
// 已结束请求的裁剪由会话侧按会话容量上限负责, 此处不再按请求条数裁剪。
func (r *RequestState) finishLocked(usage *llm.Usage) {
	r.Sending = false
	r.cancel = nil
	if usage != nil {
		r.Usage = *usage
	}
	// 未结束的最后一轮在此补齐耗时, 使被取消的那一轮也能显示实际等待时间。
	if count := len(r.Rounds); count > 0 && r.Rounds[count-1].DurationMS == 0 {
		r.Rounds[count-1].DurationMS = time.Since(r.Rounds[count-1].StartedAt).Milliseconds()
	}
	metrics := usageMetrics(r.TargetModel, usage)
	r.Cost = metrics.InputCost + metrics.OutputCost
	r.Duration = time.Since(r.StartedAt)
	metrics.WaitTime = r.Duration.Milliseconds()
	if r.Status == StatusSuccess {
		metrics.RequestSuccess = 1
	} else {
		metrics.RequestFailed = 1
	}
	_ = op.StatsTotalUpdate(metrics)
	_ = op.StatsHourlyUpdate(metrics)
	_ = op.StatsDailyUpdate(context.Background(), metrics)
	if r.apiKeyID > 0 {
		_ = op.StatsAPIKeyUpdate(r.apiKeyID, metrics)
	}
	publishRequestLocked(r)
}

// usageMetrics 将统一用量按模型单价转换为 Token 与费用统计; 无用量或价格时对应费用为零。
func usageMetrics(modelName string, usage *llm.Usage) model.StatsMetrics {
	if usage == nil {
		return model.StatsMetrics{}
	}
	metrics := model.StatsMetrics{InputToken: usage.PromptTokens, OutputToken: usage.CompletionTokens}
	price, err := op.LLMGet(modelName)
	if err != nil {
		return metrics
	}
	cachedTokens, writeCachedTokens := int64(0), int64(0)
	if usage.PromptTokensDetails != nil {
		cachedTokens = usage.PromptTokensDetails.CachedTokens
		writeCachedTokens = usage.PromptTokensDetails.WriteCachedTokens
	}
	inputTokens := max(int64(0), usage.PromptTokens-cachedTokens-writeCachedTokens)
	metrics.InputCost = (float64(inputTokens)*price.Input + float64(cachedTokens)*price.CacheRead + float64(writeCachedTokens)*price.CacheWrite) / 1_000_000
	metrics.OutputCost = float64(usage.CompletionTokens) * price.Output / 1_000_000
	return metrics
}

// requestMessage 复制请求状态用于发布; Rounds 按值复制以免前端读到后续变更, 也避免与写入方共享底层数组。
func requestMessage(request *RequestState) RequestState {
	message := *request
	message.Rounds = slices.Clone(request.Rounds)
	return message
}

// publishRequestLocked 非阻塞发布最新请求状态, 连接拥塞时关闭它并交给客户端重连获取全量快照; 调用方必须持有锁。
func publishRequestLocked(request *RequestState) {
	message := requestMessage(request)
	for stream := range watchers {
		select {
		case stream <- message:
		default:
			delete(watchers, stream)
			close(stream)
		}
	}
}

// OpenRequestStream 注册请求状态流连接, 返回按请求 ID 倒序的全部快照和后续增量通道。
func OpenRequestStream() ([]RequestState, chan RequestState) {
	mu.Lock()
	defer mu.Unlock()

	stream := make(chan RequestState, streamBuffer)
	watchers[stream] = struct{}{}

	snapshot := make([]RequestState, 0, len(requests))
	for _, request := range requests {
		snapshot = append(snapshot, requestMessage(request))
	}
	sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].ID > snapshot[j].ID })
	return snapshot, stream
}

// CloseRequestStream 注销并关闭指定请求状态流连接。
func CloseRequestStream(stream chan RequestState) {
	mu.Lock()
	defer mu.Unlock()

	if _, exists := watchers[stream]; exists {
		delete(watchers, stream)
		close(stream)
	}
}

// RequestBody 返回指定请求保存的原始请求体, 记录不存在时返回空串。
func RequestBody(id uint64) string {
	mu.Lock()
	defer mu.Unlock()

	if request := requests[id]; request != nil {
		return request.body
	}
	return ""
}

// ResponseBody 返回指定请求当前保存的响应体, 记录不存在或响应未完成时返回空串。
func ResponseBody(id uint64) string {
	mu.Lock()
	defer mu.Unlock()

	if request := requests[id]; request != nil {
		return request.responseBody
	}
	return ""
}

// Clear 删除全部已结束的请求记录及其所属的空闲会话。
func Clear() {
	mu.Lock()
	for id, request := range requests {
		if request.Status != StatusRunning && request.Status != StatusCommitted {
			delete(requests, id)
		}
	}
	mu.Unlock()

	SessionClear()
}
