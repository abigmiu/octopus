package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/gin-contrib/sse"
	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
	"github.com/tidwall/sjson"
)

// Forward 按客户端协议承载一个请求的完整转发过程。
// 目标由会话绑定给出: 每个新会话都要人工选择一次渠道和模型, 未选择前该会话的请求一直等待。
// 客户端请求的 model 字段不参与选路, 只作为展示信息记录。
func Forward(format llm.APIFormat) gin.HandlerFunc {
	var inbound transformer.Inbound
	switch format {
	case llm.APIFormatOpenAIResponse:
		inbound = responses.NewInboundTransformer()
	case llm.APIFormatAnthropicMessage:
		inbound = anthropic.NewInboundTransformer()
	default:
		inbound = openai.NewInboundTransformer()
	}

	return func(c *gin.Context) {
		// 完整读取客户端请求, 正文先登记到请求状态, 后续每轮直接改写为当前目标请求。
		raw, err := httpclient.ReadHTTPRequest(c.Request)
		if err != nil {
			rejectRequest(c, inbound, err)
			return
		}

		// 此处只读取展示和分流所需字段; 完整协议校验由同协议上游或跨协议 pipeline 完成。
		var metadata struct {
			Model     string `json:"model"`  // 客户端请求的模型名称, 仅用于展示。
			Streaming bool   `json:"stream"` // 客户端是否请求流式响应。
		}
		if err := json.Unmarshal(raw.Body, &metadata); err != nil {
			rejectRequest(c, inbound, err)
			return
		}

		// 会话身份由客户端信号或请求内容派生, 是本请求唯一的选路依据。
		identity := identifySession(c.Request.Header, raw.Body)
		if identity.id == "" {
			// 没有会话身份就无法要求人工为该会话选择目标, 也不能沿用别的会话的绑定。
			rejectRequest(c, inbound, errors.New("cannot identify a client session for this request"))
			return
		}
		sessionID := identity.id

		// 登记进程内请求状态, 返回的记录是后续全部状态写入和前端可视化推送的入口。
		request := newRequestState(metadata.Model, string(raw.Body), c.GetInt("api_key_id"))
		request.setSession(sessionID, identity.label)
		registerSessionRequest(identity, metadata.Model, request.ID)

		ctx := c.Request.Context()
		failedTarget := sessionTarget{} // 当前累计连续失败次数的目标。
		failures := 0                   // 该目标包含首次请求的连续失败次数。

		for {
			if ctx.Err() != nil {
				request.markCanceled(clientCanceledReason, "", nil)
				return
			}

			// 转发配置随时可改, 故每轮重新读取。
			config := op.RelayConfigGet()

			// 尚未绑定时阻塞等待人工选择, 期间该会话在前端显示为待选。
			target, ok, waitErr := awaitSessionTarget(ctx, identity)
			if !ok {
				if ctx.Err() != nil {
					request.markCanceled(clientCanceledReason, "", nil)
					return
				}
				if waitErr == nil {
					waitErr = errors.New("no channel selected for this session")
				}
				recordSessionFailure(sessionID, waitErr.Error())
				request.markFailed(waitErr, "", nil)
				rejectRequest(c, inbound, waitErr)
				return
			}

			// 绑定指向的渠道已被删除时等待, 期间人工改绑到别的渠道即可让请求继续。
			channel, err := op.ChannelGet(target.channelID)
			if err != nil {
				recordSessionFailure(sessionID, err.Error())
				if !request.wait(ctx, config.RetryIntervalSeconds) {
					return
				}
				continue
			}

			// 将绑定的真实模型写入本轮上游请求。
			raw.Body, err = sjson.SetBytes(raw.Body, "model", target.modelName)
			if err != nil {
				request.markFailed(err, "", nil)
				rejectRequest(c, inbound, err)
				return
			}
			// OpenAI Chat 流式响应需显式要求上游在末尾附带用量。
			if metadata.Streaming && format == llm.APIFormatOpenAIChatCompletion {
				raw.Body, err = sjson.SetBytes(raw.Body, "stream_options.include_usage", true)
				if err != nil {
					request.markFailed(err, "", nil)
					rejectRequest(c, inbound, err)
					return
				}
			}

			// 首个有效响应的等待时限由全局配置给出: 超时以明确原因结束本轮, 而不是把请求挂到客户端自己放弃。
			responseTimeout := time.Duration(config.ResponseTimeoutSeconds) * time.Second
			if metadata.Streaming {
				responseTimeout = time.Duration(config.StreamFirstEventTimeoutSeconds) * time.Second
			}
			// 为本轮上游调用建立独立取消入口并登记当前目标。
			roundCtx, cancelRound := context.WithCancel(ctx)
			request.startRound(cancelRound, channel.Name, target.modelName)

			// 按渠道协议构造出站转换器并确定是否可以直接透传。
			roundStartedAt := time.Now() // 本轮上游调用的开始时间, 用于统计首个有效响应耗时。
			outbound, passthrough, err := buildOutbound(channel, format)

			// 请求上游并等待首个有效响应: 非流式等待完整响应, 流式等待首个事件。
			// 同协议渠道原样直通, 跨协议渠道经转换后请求; 此时尚未写给客户端, 失败仍可重试。
			var result *upstreamResponse
			if err == nil {
				if passthrough {
					result, err = sendPassthrough(roundCtx, format, raw, channel, outbound, metadata.Streaming, responseTimeout)
				} else {
					result, err = sendConverted(roundCtx, format, raw, channel, outbound, metadata.Streaming, responseTimeout)
				}
			}

			if err != nil {
				// 记录本轮上游调用已经结束及其失败原因。
				request.finishRound(err.Error(), ctx.Err() != nil)
				// 父上下文结束说明客户端已经取消。
				if ctx.Err() != nil {
					cancelRound()
					request.markCanceled(clientCanceledReason, "", nil)
					return
				}
				// 仅本轮上下文结束说明绑定被人工改动或该轮被人工中止, 不计失败也不等待, 立即按新绑定重试。
				if roundCtx.Err() != nil {
					cancelRound()
					failedTarget = sessionTarget{}
					failures = 0
					continue
				}
				cancelRound()
				// 本轮真实失败只计入当前渠道和模型, 客户端取消与人工中止不计为渠道故障。
				metrics := model.StatsMetrics{WaitTime: time.Since(roundStartedAt).Milliseconds(), RequestFailed: 1}
				_ = op.StatsChannelUpdate(channel.ID, metrics)
				_ = op.StatsModelUpdate(channel.ID, target.modelName, metrics)
				recordSessionFailure(sessionID, err.Error())

				// 目标由人工绑定, 失败不自动换渠道: 按配置重试同一目标, 耗尽后失败并保留绑定等待人工换目标。
				if failedTarget == target {
					failures++
				} else {
					failedTarget = target
					failures = 1
				}
				if failures >= config.MaxAttempts {
					request.markFailed(err, "", nil)
					rejectRequest(c, inbound, err)
					return
				}
				if !request.wait(ctx, config.RetryIntervalSeconds) {
					return
				}
				continue
			}
			// 记录本轮已经取得可提交的上游响应。
			request.finishRound("", false)
			roundWaitTime := time.Since(roundStartedAt).Milliseconds() // 流式响应只统计等待首帧的时间。
			// 同协议透传时原样返回上游响应头; 跨协议响应没有需要透传的响应头。
			for key, values := range result.header {
				c.Writer.Header()[key] = values
			}

			// 非流式响应已经完整取得, 提交后一次写给客户端。
			if !metadata.Streaming {
				cancelRound()
				if c.Writer.Header().Get("Content-Type") == "" {
					c.Header("Content-Type", "application/json")
				}
				// 非流式响应已有完整用量, 本轮渠道和模型统计可在提交前一次完成。
				metrics := usageMetrics(target.modelName, result.usage)
				metrics.WaitTime = roundWaitTime
				metrics.RequestSuccess = 1
				_ = op.StatsChannelUpdate(channel.ID, metrics)
				_ = op.StatsModelUpdate(channel.ID, target.modelName, metrics)
				recordSessionSuccess(sessionID, result.usage, metrics)
				request.markCommitted()
				n, err := c.Writer.Write(result.body)
				if err == nil && n != len(result.body) {
					err = io.ErrShortWrite
				}
				if err != nil {
					if ctx.Err() != nil {
						request.markCanceled(writeCanceledReason, string(result.body), result.usage)
					} else {
						request.markFailed(err, string(result.body), result.usage)
					}
					return
				}
				request.markSucceeded(string(result.body), result.usage)
				return
			}

			// 首帧提交后仍需逐个事件判断协议终态: 上游发出结束事件后未必立即关闭响应体, 继续读取会一直阻塞到
			// 客户端断开, 从而把已完整交付的响应误判为 context canceled。
			if c.Writer.Header().Get("Content-Type") == "" {
				c.Header("Content-Type", "text/event-stream")
			}
			var encoded bytes.Buffer
			var chunks []*httpclient.StreamEvent
			event := result.first
			last := result.last // 已转发的最后一个事件是否已按客户端协议结束整个响应流。
			committed := false
			for {
				if event != nil {
					chunks = append(chunks, event)
					encoded.Reset()
					if encodeErr := sse.Encode(&encoded, sse.Event{Id: event.LastEventID, Event: event.Type, Data: event.Data}); encodeErr != nil {
						err = encodeErr
						break
					}
					if !committed {
						request.markCommitted()
						committed = true
					}
					n, writeErr := c.Writer.Write(encoded.Bytes())
					if writeErr == nil && n != encoded.Len() {
						writeErr = io.ErrShortWrite
					}
					if writeErr != nil {
						err = writeErr
						break
					}
					c.Writer.Flush()
				}
				if last {
					break
				}
				if !result.events.Next() {
					err = result.events.Err()
					break
				}
				event = result.events.Current()
				// 已提交的响应不能再重试, 结束事件自身携带的失败原样转发给客户端, 并在转发后作为本请求终态。
				last, err = inspectStreamEvent(format, event)
			}
			result.events.Close()
			cancelRound()
			// 使用客户端协议转换器聚合已转发事件, 统一取得最终响应正文和用量。
			responseBody, meta, aggregateErr := inbound.AggregateStreamChunks(context.WithoutCancel(ctx), chunks)
			if aggregateErr == nil {
				result.usage = meta.Usage
			}
			// 流式响应结束并聚合出用量后, 按最终结果完成本轮渠道和模型统计。
			metrics := usageMetrics(target.modelName, result.usage)
			metrics.WaitTime = roundWaitTime
			if err == nil {
				metrics.RequestSuccess = 1
			} else {
				metrics.RequestFailed = 1
			}
			_ = op.StatsChannelUpdate(channel.ID, metrics)
			_ = op.StatsModelUpdate(channel.ID, target.modelName, metrics)
			if err != nil {
				recordSessionFailure(sessionID, err.Error())
				if ctx.Err() != nil {
					request.markCanceled(writeCanceledReason, string(responseBody), result.usage)
				} else {
					request.markFailed(err, string(responseBody), result.usage)
				}
				return
			}
			recordSessionSuccess(sessionID, result.usage, metrics)
			request.markSucceeded(string(responseBody), result.usage)
			return
		}
	}
}

// rejectRequest 以客户端协议的错误格式返回请求级失败, 用于尚未登记状态因而无需定稿的请求。
func rejectRequest(c *gin.Context, inbound transformer.Inbound, err error) {
	response := inbound.TransformError(c.Request.Context(), &llm.ResponseError{
		StatusCode: http.StatusBadRequest,
		Detail:     llm.ErrorDetail{Message: err.Error(), Type: "invalid_request_error"},
	})
	c.Data(response.StatusCode, "application/json", response.Body)
	c.Abort()
}
