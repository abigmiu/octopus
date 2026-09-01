package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync/atomic"
	"time"

	"github.com/bestruirui/octopus/internal/helper"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
	"github.com/tidwall/sjson"
)

// upstreamResponse 是已验证但尚未写给客户端的上游成功响应; events 为 nil 表示非流式响应。
// 透传响应保留上游响应头; 跨协议响应由客户端协议决定响应头。失败一律以 error 返回。
type upstreamResponse struct {
	body   []byte                                  // 非流式响应的完整正文。
	header http.Header                             // 同协议透传时需要原样返回的上游响应头。
	events streams.Stream[*httpclient.StreamEvent] // 流式响应中首个事件之后的剩余事件。
	first  *httpclient.StreamEvent                 // 已预读并验证的首个事件。
	last   bool                                    // 首个事件已经终止整个响应流。
	usage  *llm.Usage                              // 上游本次可确认的用量。
}

// firstResponseGuard 在取得首个有效响应之前限制等待时间: 超时则取消上游调用, 取得响应后调用 stop 停止计时,
// 因此流式响应的后续事件不受该时限约束。expired 报告超时是否已经发生, 用于把上下文取消归因为上游超时。
// 返回的上下文不需要单独释放, 它随调用方给出的父上下文一起结束。
func firstResponseGuard(ctx context.Context, timeout time.Duration) (guarded context.Context, stop func(), expired func() bool) {
	guarded, cancel := context.WithCancel(ctx)
	var timedOut atomic.Bool
	timer := time.AfterFunc(timeout, func() {
		timedOut.Store(true)
		cancel()
	})
	return guarded, func() { timer.Stop() }, timedOut.Load
}

// sendPassthrough 以同协议透传方式请求上游, 取得的响应无需转换即可回给客户端。
// firstResponseTimeout 只约束到取得首个有效响应, 流式响应此后的事件不再计时。
func sendPassthrough(ctx context.Context, format llm.APIFormat, raw *httpclient.Request, channel model.Channel, outbound transformer.Outbound, streaming bool, firstResponseTimeout time.Duration) (*upstreamResponse, error) {
	request, err := buildPassthroughRequest(format, raw, channel)
	if err != nil {
		return nil, err
	}
	client, err := helper.ChannelHttpClient(&channel)
	if err != nil {
		return nil, err
	}

	guarded, stop, expired := firstResponseGuard(ctx, firstResponseTimeout)
	result, err := func() (*upstreamResponse, error) {
		if streaming {
			return sendPassthroughStream(guarded, format, request, client)
		}

		response, err := httpclient.NewHttpClientWithClient(client).Do(guarded, request)
		if err != nil {
			var failure *httpclient.Error
			if errors.As(err, &failure) && len(failure.Body) > 0 {
				return nil, fmt.Errorf("%w: %s", err, failure.Body)
			}
			return nil, err
		}
		// 同协议下响应可原样回给客户端, 仍需解析一次以取得用量并识别以 200 下发的失败终态。
		parsed, err := outbound.TransformResponse(guarded, response)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", err, response.Body)
		}
		if err := validateResponse(format, parsed); err != nil {
			return nil, fmt.Errorf("%w: %s", err, response.Body)
		}
		return &upstreamResponse{body: slices.Clone(response.Body), header: response.Headers.Clone(), usage: parsed.Usage}, nil
	}()
	if err == nil {
		stop()
		return result, nil
	}
	if expired() {
		return nil, fmt.Errorf("upstream did not respond within %s: %w", firstResponseTimeout, err)
	}
	return nil, err
}

// sendPassthroughStream 发起同协议流式请求并预读首个有效事件, 首个事件通过验证才算本轮取得可提交响应。
func sendPassthroughStream(ctx context.Context, format llm.APIFormat, request *httpclient.Request, client *http.Client) (*upstreamResponse, error) {
	rawRequest, err := httpclient.BuildHttpRequest(ctx, request)
	if err != nil {
		return nil, err
	}
	// 客户端的 Accept 属于库自管头不会透传, 需显式声明才能让上游按 SSE 返回。
	rawRequest.Header.Set("Accept", "text/event-stream")

	response, err := client.Do(rawRequest)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= http.StatusBadRequest {
		failure, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		return nil, fmt.Errorf("upstream responded %s: %s", response.Status, failure)
	}

	events := httpclient.NewDefaultSSEDecoder(ctx, response.Body)
	for events.Next() {
		event := events.Current()
		if event == nil || len(event.Data) == 0 {
			continue
		}
		last, err := inspectStreamEvent(format, event)
		if err != nil {
			events.Close()
			return nil, fmt.Errorf("%w: %s", err, event.Data)
		}
		return &upstreamResponse{header: response.Header.Clone(), events: events, first: event, last: last}, nil
	}

	err = events.Err()
	events.Close()
	if err == nil {
		err = errors.New("upstream stream ended before first event")
	}
	return nil, err
}

// conversionMiddleware 保存跨协议 pipeline 单次调用需要应用和取得的状态。
type conversionMiddleware struct {
	pipeline.DummyMiddleware               // 提供本次无需处理的其余 pipeline 中间件方法。
	channel                  model.Channel // 本轮上游请求使用的渠道配置。
	format                   llm.APIFormat // 上游渠道协议, 用于校验统一响应终态。
	rawBody                  []byte        // 上游非流式响应或错误的原始正文。
	usage                    *llm.Usage    // 非流式统一响应中确认的用量。
}

// OnOutboundRawRequest 剔除不属于渠道协议的客户端专属 Header, 再应用渠道参数和自定义 Header。
func (m *conversionMiddleware) OnOutboundRawRequest(_ context.Context, request *httpclient.Request) (*httpclient.Request, error) {
	stripForeignProtocolHeaders(m.format, request.Headers)
	return request, applyChannelConfig(m.channel, request)
}

// OnOutboundRawError 保留上游错误状态码携带的原始正文。
func (m *conversionMiddleware) OnOutboundRawError(_ context.Context, err error) {
	var failure *httpclient.Error
	if errors.As(err, &failure) {
		m.rawBody = slices.Clone(failure.Body)
	}
}

// OnOutboundRawResponse 保留上游成功响应的原始正文, 供后续转换或终态校验失败时诊断。
func (m *conversionMiddleware) OnOutboundRawResponse(_ context.Context, response *httpclient.Response) (*httpclient.Response, error) {
	m.rawBody = slices.Clone(response.Body)
	return response, nil
}

// OnOutboundLlmResponse 取得非流式用量并在回转客户端协议前校验上游终态。
func (m *conversionMiddleware) OnOutboundLlmResponse(_ context.Context, response *llm.Response) (*llm.Response, error) {
	if err := validateResponse(m.format, response); err != nil {
		return nil, err
	}
	m.usage = response.Usage
	return response, nil
}

// sendConverted 经 axonhub pipeline 把客户端请求转换成渠道协议后请求上游, 响应再转换回客户端协议。
// firstResponseTimeout 只约束到取得首个有效响应, 流式响应此后的事件不再计时。
func sendConverted(ctx context.Context, format llm.APIFormat, raw *httpclient.Request, channel model.Channel, outbound transformer.Outbound, streaming bool, firstResponseTimeout time.Duration) (*upstreamResponse, error) {
	var inbound transformer.Inbound
	switch format {
	case llm.APIFormatOpenAIResponse:
		inbound = responses.NewInboundTransformer()
	case llm.APIFormatAnthropicMessage:
		inbound = anthropic.NewInboundTransformer()
	default:
		inbound = openai.NewInboundTransformer()
	}

	// Codex 把工具声明放在 input[].additional_tools, 跨协议转换器不识别这类 item 会整体丢弃;
	// 提前提取并提升到顶层 tools 才能让下游拿到工具。custom 工具也一并降级为 function。
	if format == llm.APIFormatOpenAIResponse {
		normalized, err := normalizeResponsesTools(raw.Body)
		if err != nil {
			return nil, err
		}
		raw.Body = normalized
	}

	client, err := helper.ChannelHttpClient(&channel)
	if err != nil {
		return nil, err
	}
	middleware := &conversionMiddleware{channel: channel, format: outbound.APIFormat()}
	processor := pipeline.NewFactory(httpclient.NewHttpClientWithClient(client)).Pipeline(
		inbound,
		outbound,
		pipeline.WithMiddlewares(middleware),
		// pipeline 内部同样按该时限等待首个响应, 与外层守护共同确保上游卡住时能以明确原因结束本轮。
		pipeline.WithResponseTimeouts(firstResponseTimeout, firstResponseTimeout),
	)

	guarded, stop, expired := firstResponseGuard(ctx, firstResponseTimeout)
	result, err := func() (*upstreamResponse, error) {
		result, err := processor.Process(guarded, raw)
		if err != nil {
			if len(middleware.rawBody) > 0 {
				return nil, fmt.Errorf("%w: %s", err, middleware.rawBody)
			}
			return nil, err
		}
		if !streaming {
			return &upstreamResponse{body: slices.Clone(result.Response.Body), usage: middleware.usage}, nil
		}

		events := result.EventStream
		for events.Next() {
			event := events.Current()
			if event == nil || len(event.Data) == 0 {
				continue
			}
			last, err := inspectStreamEvent(format, event)
			if err != nil {
				events.Close()
				return nil, fmt.Errorf("%w: %s", err, event.Data)
			}
			return &upstreamResponse{events: events, first: event, last: last}, nil
		}

		err = events.Err()
		events.Close()
		if err == nil {
			err = errors.New("upstream stream ended before first event")
		}
		return nil, err
	}()
	if err == nil {
		stop()
		return result, nil
	}
	if expired() {
		return nil, fmt.Errorf("upstream did not respond within %s: %w", firstResponseTimeout, err)
	}
	return nil, err
}

// normalizeResponsesTools 处理 Codex 风格的 Responses 请求: 把 input 中 additional_tools 声明的
// 工具目录提升到顶层 tools, 展开 namespace, 并把 custom 工具降级为 function。
// 跨协议转换(如 Responses->Anthropic)时, 不处理这些工具会被 axonhub 转换器整体丢弃。
func normalizeResponsesTools(body []byte) ([]byte, error) {
	var payload struct {
		Input []json.RawMessage `json:"input"`
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}

	var promoted []json.RawMessage
	for _, itemRaw := range payload.Input {
		var item struct {
			Type  string            `json:"type"`
			Tools []json.RawMessage `json:"tools"`
		}
		if err := json.Unmarshal(itemRaw, &item); err != nil || item.Type != "additional_tools" {
			continue
		}
		for _, toolRaw := range item.Tools {
			var tool struct {
				Type  string            `json:"type"`
				Name  string            `json:"name"`
				Tools []json.RawMessage `json:"tools"`
			}
			if err := json.Unmarshal(toolRaw, &tool); err != nil {
				continue
			}
			switch tool.Type {
			case "namespace":
				promoted = append(promoted, expandNamespaceTool(tool.Name, tool.Tools)...)
			case "function":
				promoted = append(promoted, toolRaw)
			case "custom":
				if downgraded, err := downgradeCustomTool(toolRaw, tool.Name); err == nil {
					promoted = append(promoted, downgraded)
				}
			}
		}
	}
	if len(promoted) == 0 {
		return body, nil
	}

	merged := append(payload.Tools, promoted...)
	mergedRaw, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(body, "tools", mergedRaw)
}

// expandNamespaceTool 把 namespace 子工具展开为 namespace__name 的顶层工具, custom 子工具降级为 function。
func expandNamespaceTool(namespace string, subTools []json.RawMessage) []json.RawMessage {
	var out []json.RawMessage
	for _, subRaw := range subTools {
		var sub struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(subRaw, &sub); err != nil || sub.Name == "" {
			continue
		}
		switch sub.Type {
		case "function":
			var m map[string]any
			if err := json.Unmarshal(subRaw, &m); err != nil {
				continue
			}
			m["name"] = namespace + "__" + sub.Name
			if raw, err := json.Marshal(m); err == nil {
				out = append(out, raw)
			}
		case "custom":
			if raw, err := downgradeCustomTool(subRaw, namespace+"__"+sub.Name); err == nil {
				out = append(out, raw)
			}
		}
	}
	return out
}

// downgradeCustomTool 把 Responses custom 工具降级为 function 工具: grammar 移入描述, 参数固定为 input 字符串。
// 上游(含 Anthropic 与多数 Responses 网关)只识别 function 工具, 否则 custom 会被静默丢弃。
func downgradeCustomTool(toolRaw json.RawMessage, name string) (json.RawMessage, error) {
	var tool struct {
		Description string `json:"description"`
		Format      *struct {
			Definition string `json:"definition"`
		} `json:"format"`
	}
	if err := json.Unmarshal(toolRaw, &tool); err != nil {
		return nil, err
	}
	desc := tool.Description
	if tool.Format != nil && tool.Format.Definition != "" {
		if desc != "" {
			desc += "\n"
		}
		desc += "Original grammar is advisory: " + tool.Format.Definition
	}
	return json.Marshal(map[string]any{
		"type":        "function",
		"name":        name,
		"description": desc,
		"parameters": map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"input": map[string]any{"type": "string"}},
			"required":             []string{"input"},
			"additionalProperties": false,
		},
	})
}
