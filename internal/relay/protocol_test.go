package relay

import (
	"net/http"
	"testing"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

// statsMetricsForTest 构造只带费用的统计增量。
func statsMetricsForTest(cost float64) model.StatsMetrics {
	return model.StatsMetrics{InputCost: cost}
}

// promptDetailsForTest 构造只带缓存命中数的用量明细。
func promptDetailsForTest(cached int64) *llm.PromptTokensDetails {
	return &llm.PromptTokensDetails{CachedTokens: cached}
}

// 跨协议转发到 Anthropic 上游时，OpenAI 侧专属头必须剔除，Anthropic 自身的头必须保留。
func TestStripForeignProtocolHeadersToAnthropic(t *testing.T) {
	headers := http.Header{}
	headers.Set("Anthropic-Version", "2023-06-01")
	headers.Set("Anthropic-Beta", "tools-2024")
	headers.Set("Openai-Organization", "org-1")
	headers.Set("X-Stainless-Lang", "js")
	headers.Set("Chatgpt-Account-Id", "acct")
	headers.Set("User-Agent", "codex/1.0")
	headers.Set("X-App", "cli")
	headers.Set("X-Custom-Trace", "keep-me")

	stripForeignProtocolHeaders(llm.APIFormatAnthropicMessage, headers)

	for _, name := range []string{"Anthropic-Version", "Anthropic-Beta", "X-Custom-Trace"} {
		if headers.Get(name) == "" {
			t.Fatalf("%s should be kept", name)
		}
	}
	for _, name := range []string{"Openai-Organization", "X-Stainless-Lang", "Chatgpt-Account-Id", "User-Agent", "X-App"} {
		if headers.Get(name) != "" {
			t.Fatalf("%s should be stripped", name)
		}
	}
}

// 跨协议转发到 OpenAI 上游时，Anthropic 侧专属头必须剔除，OpenAI 自身的头必须保留。
func TestStripForeignProtocolHeadersToOpenAI(t *testing.T) {
	headers := http.Header{}
	headers.Set("Anthropic-Version", "2023-06-01")
	headers.Set("Anthropic-Beta", "tools-2024")
	headers.Set("Openai-Beta", "assistants=v2")
	headers.Set("X-Stainless-Lang", "python")
	headers.Set("User-Agent", "claude-cli/2.0")

	stripForeignProtocolHeaders(llm.APIFormatOpenAIChatCompletion, headers)

	if headers.Get("Openai-Beta") == "" {
		t.Fatal("Openai-Beta should be kept")
	}
	if headers.Get("X-Stainless-Lang") == "" {
		t.Fatal("X-Stainless-Lang belongs to the OpenAI family and should be kept")
	}
	for _, name := range []string{"Anthropic-Version", "Anthropic-Beta", "User-Agent"} {
		if headers.Get(name) != "" {
			t.Fatalf("%s should be stripped", name)
		}
	}
}

// 目标协议不在已知名单中（如 Gemini）时，全部来源协议专属头都要剔除。
func TestStripForeignProtocolHeadersUnknownTarget(t *testing.T) {
	headers := http.Header{}
	headers.Set("Anthropic-Version", "2023-06-01")
	headers.Set("Openai-Beta", "assistants=v2")
	headers.Set("X-Stainless-Lang", "go")
	headers.Set("X-Custom-Trace", "keep-me")

	stripForeignProtocolHeaders(llm.APIFormatGeminiContents, headers)

	if headers.Get("X-Custom-Trace") == "" {
		t.Fatal("unrelated custom header should be kept")
	}
	for _, name := range []string{"Anthropic-Version", "Openai-Beta", "X-Stainless-Lang"} {
		if headers.Get(name) != "" {
			t.Fatalf("%s should be stripped", name)
		}
	}
}

// 渠道声明的不转发名单要生效，且同名的自定义 Header 仍可写回。
func TestApplyChannelConfigHeaderBlocklist(t *testing.T) {
	blocklist := "User-Agent, X-App"
	channel := model.Channel{
		HeaderBlocklist: &blocklist,
		CustomHeader:    []model.CustomHeader{{HeaderKey: "X-App", HeaderValue: "octopus"}},
	}
	request := requestWithHeaders(map[string]string{
		"User-Agent":     "claude-cli/2.0",
		"X-App":          "cli",
		"X-Custom-Trace": "keep-me",
	})

	if err := applyChannelConfig(channel, request); err != nil {
		t.Fatalf("applyChannelConfig: %v", err)
	}
	if request.Headers.Get("User-Agent") != "" {
		t.Fatal("blocklisted User-Agent should be removed")
	}
	if got := request.Headers.Get("X-App"); got != "octopus" {
		t.Fatalf("X-App = %q, want the custom header value", got)
	}
	if request.Headers.Get("X-Custom-Trace") == "" {
		t.Fatal("unrelated header should be kept")
	}
}

// requestWithHeaders 构造只带请求头的上游请求。
func requestWithHeaders(values map[string]string) *httpclient.Request {
	headers := http.Header{}
	for key, value := range values {
		headers.Set(key, value)
	}
	return &httpclient.Request{Headers: headers}
}
