package model

// 分组选择上游成员的模式。
type GroupMode string

const (
	GroupModeManual   GroupMode = "manual"   // 只使用人工选中的成员。
	GroupModeFailover GroupMode = "failover" // 按成员排序选择并在失败时切换。
	GroupModeSession  GroupMode = "session"  // 每个新会话由人工选择渠道和模型, 同一会话的后续请求沿用该选择。
)

// 分组 Relay 的持久化配置，数据库中以 JSON 存储。
type GroupRelayConfig struct {
	MemberMaxAttempts                     int `json:"member_max_attempts" binding:"omitempty,min=1"`                        // 单个成员包含首次请求的总尝试次数，仅在故障转移模式生效。
	MemberRetryIntervalSeconds            int `json:"member_retry_interval_seconds" binding:"omitempty,min=1"`              // 同一成员相邻两次尝试之间的等待秒数。
	MemberNonStreamResponseTimeoutSeconds int `json:"member_non_stream_response_timeout_seconds" binding:"omitempty,min=1"` // 单个成员返回完整非流式响应的超时秒数。
	MemberStreamFirstEventTimeoutSeconds  int `json:"member_stream_first_event_timeout_seconds" binding:"omitempty,min=1"`  // 单个成员返回首个有效流事件的超时秒数。
	MemberCooldownSeconds                 int `json:"member_cooldown_seconds" binding:"omitempty,min=1"`                    // 单个成员耗尽尝试后被跳过的秒数，仅在故障转移模式生效。
	MemberAffinitySeconds                 int `json:"member_affinity_seconds" binding:"omitempty,min=0"`                    // 成员亲和时间:故障切换成功后继续保持当前成员的秒数;当前成员失败会立即结束亲和,0 表示不保持。
}

// DefaultGroupRelayConfig 返回新分组使用的 Relay 默认配置。
func DefaultGroupRelayConfig() GroupRelayConfig {
	return GroupRelayConfig{
		MemberMaxAttempts:                     2,
		MemberRetryIntervalSeconds:            3,
		MemberNonStreamResponseTimeoutSeconds: 120,
		MemberStreamFirstEventTimeoutSeconds:  30,
		MemberCooldownSeconds:                 60,
		MemberAffinitySeconds:                 300,
	}
}

// NormalizeGroupRelayConfig 补齐分组 Relay 配置中的空值。
func NormalizeGroupRelayConfig(config *GroupRelayConfig) {
	defaults := DefaultGroupRelayConfig()
	if *config == (GroupRelayConfig{}) {
		*config = defaults
		return
	}
	if config.MemberMaxAttempts < 1 {
		config.MemberMaxAttempts = defaults.MemberMaxAttempts
	}
	if config.MemberRetryIntervalSeconds < 1 {
		config.MemberRetryIntervalSeconds = defaults.MemberRetryIntervalSeconds
	}
	if config.MemberNonStreamResponseTimeoutSeconds < 1 {
		config.MemberNonStreamResponseTimeoutSeconds = defaults.MemberNonStreamResponseTimeoutSeconds
	}
	if config.MemberStreamFirstEventTimeoutSeconds < 1 {
		config.MemberStreamFirstEventTimeoutSeconds = defaults.MemberStreamFirstEventTimeoutSeconds
	}
	if config.MemberCooldownSeconds < 1 {
		config.MemberCooldownSeconds = defaults.MemberCooldownSeconds
	}
	if config.MemberAffinitySeconds < 0 {
		config.MemberAffinitySeconds = defaults.MemberAffinitySeconds
	}
}

// 客户端模型名称及其可手动选择或故障转移的上游分组。
type Group struct {
	ID           int              `json:"id" gorm:"primaryKey"`                                                                  // 分组主键。
	Name         string           `json:"name" gorm:"unique;not null"`                                                           // 客户端请求使用的模型名称。
	Mode         GroupMode        `json:"mode" gorm:"not null;default:manual" binding:"omitempty,oneof=manual failover session"` // 选择成员的模式。
	ActiveItemID int              `json:"active_item_id" gorm:"not null;default:0"`                                              // 手动模式指定的成员，故障转移模式保留但忽略该值，0 表示未指定。
	RelayConfig  GroupRelayConfig `json:"relay_config" gorm:"serializer:json"`                                                   // 该分组的 Relay 路由配置。
	Items        []GroupItem      `json:"items,omitempty" gorm:"foreignKey:GroupID"`                                             // 该分组可手动选择或故障转移的渠道模型。
}

// 分组内一个可选择的渠道模型。
type GroupItem struct {
	ID        int    `json:"id" gorm:"primaryKey"`                                            // 分组项主键。
	GroupID   int    `json:"group_id" gorm:"not null;index:idx_group_channel_model,unique"`   // 所属分组 ID，创建分组时无需携带，更新时需要。
	ChannelID int    `json:"channel_id" gorm:"not null;index:idx_group_channel_model,unique"` // 实际上游渠道 ID。
	ModelName string `json:"model_name" gorm:"not null;index:idx_group_channel_model,unique"` // 该渠道实际请求的模型名称。
	Priority  int    `json:"priority"`                                                        // Priority 决定界面展示和故障转移模式下的成员切换顺序。
}

// 分组普通配置和成员变更请求。
type GroupUpdateRequest struct {
	ID            int                      `json:"id" binding:"required"`                                            // 待更新的分组主键。
	Name          *string                  `json:"name,omitempty"`                                                   // Name 仅在名称变更时发送。
	Mode          *GroupMode               `json:"mode,omitempty" binding:"omitempty,oneof=manual failover session"` // Mode 仅在选择模式变更时发送。
	RelayConfig   *GroupRelayConfig        `json:"relay_config,omitempty"`                                           // RelayConfig 仅在 Relay 配置变更时发送完整配置。
	ItemsToAdd    []GroupItemAddRequest    `json:"items_to_add,omitempty"`                                           // 待新增的分组项。
	ItemsToUpdate []GroupItemUpdateRequest `json:"items_to_update,omitempty"`                                        // 待调整展示和故障转移顺序的分组项。
	ItemsToDelete []int                    `json:"items_to_delete,omitempty"`                                        // 待删除的分组项 ID。
}

// 手动模式下切换或清空分组成员的请求。
type GroupActiveItemUpdateRequest struct {
	ItemID *int `json:"item_id" binding:"required"` // 待设为当前渠道的分组项 ID，0 表示取消选择。
}

// 会话模式下为一个会话选定上游目标的请求；目标不限于分组成员，任意渠道与模型均可选择。
type SessionBindRequest struct {
	SessionID string `json:"session_id" binding:"required"` // 待绑定的会话标识。
	ChannelID int    `json:"channel_id" binding:"required"` // 选定的上游渠道 ID。
	ModelName string `json:"model_name" binding:"required"` // 该渠道实际请求的模型名称。
}

// 会话模式下解除一个会话已有绑定的请求；解除后该会话的下一条请求重新等待人工选择。
type SessionUnbindRequest struct {
	SessionID string `json:"session_id" binding:"required"` // 待解除绑定的会话标识。
}

// 新增分组项请求。
type GroupItemAddRequest struct {
	ChannelID int    `json:"channel_id" binding:"required"` // 实际上游渠道 ID。
	ModelName string `json:"model_name" binding:"required"` // 该渠道实际请求的模型名称。
	Priority  int    `json:"priority,omitempty"`            // 分组项的界面展示和故障转移顺序。
}

// 分组项展示和故障转移顺序更新请求。
type GroupItemUpdateRequest struct {
	ID       int `json:"id" binding:"required"` // 待更新的分组项主键。
	Priority int `json:"priority,omitempty"`    // 新的界面展示和故障转移顺序。
}
