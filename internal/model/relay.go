package model

// RelayConfig 是转发上游时的全局重试与超时配置, 取代原先挂在分组上的同名配置。
type RelayConfig struct {
	MaxAttempts                    int `json:"max_attempts" binding:"omitempty,min=1"`                       // 单个渠道包含首次请求的总尝试次数。
	RetryIntervalSeconds           int `json:"retry_interval_seconds" binding:"omitempty,min=1"`             // 同一渠道相邻两次尝试之间的等待秒数。
	ResponseTimeoutSeconds         int `json:"response_timeout_seconds" binding:"omitempty,min=1"`           // 等待完整非流式响应的超时秒数。
	StreamFirstEventTimeoutSeconds int `json:"stream_first_event_timeout_seconds" binding:"omitempty,min=1"` // 等待首个有效流事件的超时秒数。
}

// DefaultRelayConfig 返回转发配置的默认值。
func DefaultRelayConfig() RelayConfig {
	return RelayConfig{
		MaxAttempts:                    2,
		RetryIntervalSeconds:           3,
		ResponseTimeoutSeconds:         120,
		StreamFirstEventTimeoutSeconds: 30,
	}
}

// SessionBindRequest 为一个会话选定上游渠道和模型。
type SessionBindRequest struct {
	SessionID string `json:"session_id" binding:"required"` // 待绑定的会话标识。
	ChannelID int    `json:"channel_id" binding:"required"` // 目标渠道 ID。
	ModelName string `json:"model_name" binding:"required"` // 该渠道实际请求的模型名称。
}

// SessionUnbindRequest 解除一个会话的绑定。
type SessionUnbindRequest struct {
	SessionID string `json:"session_id" binding:"required"` // 待解除绑定的会话标识。
}
