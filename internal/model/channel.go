package model

// 渠道使用的上游服务提供方。
type ChannelProvider string

const (
	ChannelProviderOpenAI          ChannelProvider = "openai"
	ChannelProviderOpenAIResponses ChannelProvider = "openai_responses"
	ChannelProviderAnthropic       ChannelProvider = "anthropic"
	ChannelProviderGemini          ChannelProvider = "gemini"
	ChannelProviderVolcengine      ChannelProvider = "volcengine"
)

// 单个上游渠道的连接和转发配置。
type Channel struct {
	ID              int             `json:"id" gorm:"primaryKey"`                        // 渠道主键。
	Name            string          `json:"name" gorm:"unique;not null"`                 // 渠道名称。
	Type            ChannelProvider `json:"type"`                                        // 上游服务提供方。
	Enabled         bool            `json:"enabled" gorm:"default:true"`                 // 渠道是否可用。
	BaseURL         string          `json:"base_url"`                                    // 唯一的上游基础地址。
	Key             string          `json:"key"`                                         // 唯一的上游访问凭据。
	Model           string          `json:"model"`                                       // 自动同步的模型列表。
	CustomModel     string          `json:"custom_model"`                                // 手动配置的模型列表。
	Proxy           bool            `json:"proxy" gorm:"default:false"`                  // 是否使用代理。
	AutoSync        bool            `json:"auto_sync" gorm:"default:false"`              // 是否自动同步模型。
	CustomHeader    []CustomHeader  `json:"custom_header" gorm:"serializer:json"`        // 追加到上游请求的 Header。
	HeaderBlocklist *string         `json:"header_blocklist"`                            // 不转发给上游的客户端 Header 名单，逗号分隔。
	ParamOverride   *string         `json:"param_override"`                              // 请求参数覆盖配置。
	ChannelProxy    *string         `json:"channel_proxy"`                               // 渠道专用代理地址。
	Stats           *StatsChannel   `json:"stats,omitempty" gorm:"foreignKey:ChannelID"` // 渠道统计信息。
	MatchRegex      *string         `json:"match_regex"`                                 // 模型同步过滤表达式。
}

// 追加到上游请求的单个 Header。
type CustomHeader struct {
	HeaderKey   string `json:"header_key"`   // Header 名称。
	HeaderValue string `json:"header_value"` // Header 值。
}

// ChannelUpdateRequest 渠道更新请求 - 仅包含变更的数据
type ChannelUpdateRequest struct {
	ID              int              `json:"id" binding:"required"`      // 待更新渠道的主键。
	Name            *string          `json:"name,omitempty"`             // 新的渠道名称。
	Type            *ChannelProvider `json:"type,omitempty"`             // 新的上游服务提供方。
	Enabled         *bool            `json:"enabled,omitempty"`          // 新的启用状态。
	BaseURL         *string          `json:"base_url,omitempty"`         // 新的上游基础地址。
	Key             *string          `json:"key,omitempty"`              // 新的上游访问凭据。
	Model           *string          `json:"model,omitempty"`            // 新的自动同步模型列表。
	CustomModel     *string          `json:"custom_model,omitempty"`     // 新的自定义模型列表。
	Proxy           *bool            `json:"proxy,omitempty"`            // 新的代理开关。
	AutoSync        *bool            `json:"auto_sync,omitempty"`        // 新的自动同步开关。
	CustomHeader    *[]CustomHeader  `json:"custom_header,omitempty"`    // 新的自定义 Header。
	HeaderBlocklist *string          `json:"header_blocklist,omitempty"` // 新的不转发 Header 名单。
	ChannelProxy    *string          `json:"channel_proxy,omitempty"`    // 新的渠道代理地址。
	ParamOverride   *string          `json:"param_override,omitempty"`   // 新的参数覆盖配置。
	MatchRegex      *string          `json:"match_regex,omitempty"`      // 新的模型过滤表达式。
}
