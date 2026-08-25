package model

import (
	"fmt"
	"net/url"
	"strconv"
)

type SettingKey string

const (
	SettingKeyProxyURL                SettingKey = "proxy_url"
	SettingKeyStatsSaveInterval       SettingKey = "stats_save_interval"        // 将统计信息写入数据库的周期(分钟)
	SettingKeyModelInfoUpdateInterval SettingKey = "model_info_update_interval" // 模型信息更新间隔(小时)
	SettingKeySyncLLMInterval         SettingKey = "sync_llm_interval"          // LLM 同步间隔(小时)
	SettingKeyCORSAllowOrigins        SettingKey = "cors_allow_origins"         // 跨域白名单(逗号分隔, 如 "example.com,example2.com"). 为空不允许跨域, "*"允许所有

	// 转发重试与超时配置, 原先按分组保存, 分组移除后改为全局。
	SettingKeyRelayMaxAttempts        SettingKey = "relay_max_attempts"                       // 单个渠道包含首次请求的总尝试次数
	SettingKeyRelayRetryInterval      SettingKey = "relay_retry_interval_seconds"             // 同一渠道相邻两次尝试之间的等待秒数
	SettingKeyRelayResponseTimeout    SettingKey = "relay_response_timeout_seconds"           // 等待完整非流式响应的超时秒数
	SettingKeyRelayStreamFirstTimeout SettingKey = "relay_stream_first_event_timeout_seconds" // 等待首个有效流事件的超时秒数
)

type Setting struct {
	Key   SettingKey `json:"key" gorm:"primaryKey"`
	Value string     `json:"value" gorm:"not null"`
}

func DefaultSettings() []Setting {
	relay := DefaultRelayConfig()
	return []Setting{
		{Key: SettingKeyProxyURL, Value: ""},
		{Key: SettingKeyStatsSaveInterval, Value: "10"},       // 默认10分钟保存一次统计信息
		{Key: SettingKeyCORSAllowOrigins, Value: ""},          // CORS 默认不允许跨域，设置为 "*" 才允许所有来源
		{Key: SettingKeyModelInfoUpdateInterval, Value: "24"}, // 默认24小时更新一次模型信息
		{Key: SettingKeySyncLLMInterval, Value: "24"},         // 默认24小时同步一次LLM
		{Key: SettingKeyRelayMaxAttempts, Value: strconv.Itoa(relay.MaxAttempts)},
		{Key: SettingKeyRelayRetryInterval, Value: strconv.Itoa(relay.RetryIntervalSeconds)},
		{Key: SettingKeyRelayResponseTimeout, Value: strconv.Itoa(relay.ResponseTimeoutSeconds)},
		{Key: SettingKeyRelayStreamFirstTimeout, Value: strconv.Itoa(relay.StreamFirstEventTimeoutSeconds)},
	}
}

func (s *Setting) Validate() error {
	switch s.Key {
	case SettingKeyModelInfoUpdateInterval, SettingKeySyncLLMInterval:
		_, err := strconv.Atoi(s.Value)
		if err != nil {
			return fmt.Errorf("model info update interval must be an integer")
		}
		return nil
	case SettingKeyRelayMaxAttempts, SettingKeyRelayRetryInterval, SettingKeyRelayResponseTimeout, SettingKeyRelayStreamFirstTimeout:
		value, err := strconv.Atoi(s.Value)
		if err != nil || value < 1 {
			return fmt.Errorf("%s must be an integer greater than zero", s.Key)
		}
		return nil
	case SettingKeyProxyURL:
		if s.Value == "" {
			return nil
		}
		parsedURL, err := url.Parse(s.Value)
		if err != nil {
			return fmt.Errorf("proxy URL is invalid: %w", err)
		}
		validSchemes := map[string]bool{
			"http":   true,
			"https":  true,
			"socks5": true,
		}
		if !validSchemes[parsedURL.Scheme] {
			return fmt.Errorf("proxy URL scheme must be http, https, socks, or socks5")
		}
		if parsedURL.Host == "" {
			return fmt.Errorf("proxy URL must have a host")
		}
		return nil
	}

	return nil
}
