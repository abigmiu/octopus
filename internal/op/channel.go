package op

import (
	"context"
	"fmt"
	"strings"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/utils/cache"
	"github.com/bestruirui/octopus/internal/utils/xstrings"
	"github.com/charmbracelet/log"
	"gorm.io/gorm"
)

var channelCache = cache.New[int, model.Channel](16) // 渠道配置的进程内副本。

// ChannelList 返回缓存中的全部渠道。
func ChannelList() []model.Channel {
	channels := make([]model.Channel, 0, channelCache.Len())
	for _, channel := range channelCache.GetAll() {
		channels = append(channels, channel)
	}
	return channels
}

// ChannelCreate 创建渠道并写入缓存。
func ChannelCreate(channel *model.Channel, ctx context.Context) error {
	if err := db.GetDB().WithContext(ctx).Create(channel).Error; err != nil {
		return err
	}
	channelCache.Set(channel.ID, *channel)
	return nil
}

// ChannelUpdate 更新请求中明确提供的渠道字段，清理已移除模型的分组项并刷新缓存。
func ChannelUpdate(req *model.ChannelUpdateRequest, ctx context.Context) (*model.Channel, error) {
	oldChannel, ok := channelCache.Get(req.ID)
	if !ok {
		return nil, fmt.Errorf("channel not found")
	}

	var selectFields []string
	updates := model.Channel{ID: req.ID}

	if req.Name != nil {
		selectFields = append(selectFields, "name")
		updates.Name = *req.Name
	}
	if req.Type != nil {
		selectFields = append(selectFields, "type")
		updates.Type = *req.Type
	}
	if req.Enabled != nil {
		selectFields = append(selectFields, "enabled")
		updates.Enabled = *req.Enabled
	}
	if req.BaseURL != nil {
		selectFields = append(selectFields, "base_url")
		updates.BaseURL = *req.BaseURL
	}
	if req.Key != nil {
		selectFields = append(selectFields, "key")
		updates.Key = *req.Key
	}
	if req.Proxy != nil {
		selectFields = append(selectFields, "proxy")
		updates.Proxy = *req.Proxy
	}
	if req.AutoSync != nil {
		selectFields = append(selectFields, "auto_sync")
		updates.AutoSync = *req.AutoSync
	}
	if req.CustomHeader != nil {
		selectFields = append(selectFields, "custom_header")
		updates.CustomHeader = *req.CustomHeader
	}
	if req.HeaderBlocklist != nil {
		selectFields = append(selectFields, "header_blocklist")
		updates.HeaderBlocklist = req.HeaderBlocklist
	}
	if req.ChannelProxy != nil {
		selectFields = append(selectFields, "channel_proxy")
		updates.ChannelProxy = req.ChannelProxy
	}
	if req.ParamOverride != nil {
		selectFields = append(selectFields, "param_override")
		updates.ParamOverride = req.ParamOverride
	}
	if req.MatchRegex != nil {
		selectFields = append(selectFields, "match_regex")
		updates.MatchRegex = req.MatchRegex
	}

	if req.Model != nil || req.CustomModel != nil {
		models := oldChannel.Model
		customModels := oldChannel.CustomModel
		if req.Model != nil {
			models = *req.Model
		}
		if req.CustomModel != nil {
			customModels = *req.CustomModel
			selectFields = append(selectFields, "custom_model")
			updates.CustomModel = customModels
		}

		// 同名模型同时出现在自动同步与自定义列表时只保留自定义的那一份。
		customModelNames := xstrings.SplitCompact(",", customModels)
		customModelSet := make(map[string]struct{}, len(customModelNames))
		for _, modelName := range customModelNames {
			customModelSet[modelName] = struct{}{}
		}
		autoModelNames := xstrings.SplitCompact(",", models)
		filteredAutoModels := autoModelNames[:0]
		for _, modelName := range autoModelNames {
			if _, custom := customModelSet[modelName]; !custom {
				filteredAutoModels = append(filteredAutoModels, modelName)
			}
		}
		selectFields = append(selectFields, "model")
		updates.Model = strings.Join(filteredAutoModels, ",")
	}

	var channel model.Channel
	if err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if len(selectFields) > 0 {
			if err := tx.Model(&model.Channel{}).Where("id = ?", req.ID).Select(selectFields).Updates(&updates).Error; err != nil {
				return fmt.Errorf("failed to update channel: %w", err)
			}
		}
		if err := tx.First(&channel, req.ID).Error; err != nil {
			return fmt.Errorf("failed to load updated channel: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	channelCache.Set(channel.ID, channel)
	return &channel, nil
}

// ChannelEnabled 更新渠道启用状态。
func ChannelEnabled(id int, enabled bool, ctx context.Context) error {
	oldChannel, ok := channelCache.Get(id)
	if !ok {
		return fmt.Errorf("channel not found")
	}
	if err := db.GetDB().WithContext(ctx).Model(&model.Channel{}).Where("id = ?", id).Update("enabled", enabled).Error; err != nil {
		return err
	}
	oldChannel.Enabled = enabled
	channelCache.Set(id, oldChannel)
	return nil
}

// ChannelDel 删除渠道及其统计数据。
func ChannelDel(id int, ctx context.Context) error {
	_, ok := channelCache.Get(id)
	if !ok {
		return fmt.Errorf("channel not found")
	}

	err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 删除统计数据
		if err := tx.Where("channel_id = ?", id).Delete(&model.StatsChannel{}).Error; err != nil {
			return fmt.Errorf("failed to delete channel stats: %w", err)
		}
		// 模型统计按 渠道×模型 归属，随渠道一起删除。
		if err := tx.Where("channel_id = ?", id).Delete(&model.StatsModel{}).Error; err != nil {
			return fmt.Errorf("failed to delete model stats: %w", err)
		}

		// 删除渠道
		if err := tx.Delete(&model.Channel{}, id).Error; err != nil {
			return fmt.Errorf("failed to delete channel: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// 删除缓存
	channelCache.Del(id)
	statsChannelCacheNeedUpdateLock.Lock()
	statsChannelCache.Del(id)
	delete(statsChannelCacheNeedUpdate, id)
	statsChannelCacheNeedUpdateLock.Unlock()
	statsModelCacheDelByChannel(id)
	return nil
}

// ChannelLLMList 返回所有渠道暴露的模型。
func ChannelLLMList() []model.LLMChannel {
	models := []model.LLMChannel{}
	for _, channel := range channelCache.GetAll() {
		modelNames := xstrings.SplitCompact(",", channel.Model, channel.CustomModel)
		for _, modelName := range modelNames {
			models = append(models, model.LLMChannel{
				Name:        modelName,
				Enabled:     channel.Enabled,
				ChannelID:   channel.ID,
				ChannelName: channel.Name,
			})
		}
	}
	return models
}

// ChannelGet 返回指定渠道的缓存副本。
func ChannelGet(id int) (model.Channel, error) {
	channel, ok := channelCache.Get(id)
	if !ok {
		return model.Channel{}, fmt.Errorf("channel not found")
	}
	return channel, nil
}

// channelRefreshCache 从数据库刷新全部渠道缓存。
func channelRefreshCache(ctx context.Context) error {
	channels := []model.Channel{}
	if err := db.GetDB().WithContext(ctx).Find(&channels).Error; err != nil {
		log.Warnf("failed to get channels: %v", err)
		return err
	}
	for _, channel := range channels {
		channelCache.Set(channel.ID, channel)
	}
	return nil
}
