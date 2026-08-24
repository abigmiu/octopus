package op

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/utils/cache"
	"github.com/bestruirui/octopus/internal/utils/xstrings"
	"gorm.io/gorm"
)

var (
	groupCache     = cache.New[int, model.Group](16) // groupCache 按主键保存完整分组配置。
	groupNameIndex = cache.New[string, int](16)      // 客户端模型名对应的分组主键。
)

// GroupList 返回缓存中的全部分组。
func GroupList() []model.Group {
	groups := make([]model.Group, 0, groupCache.Len())
	for _, group := range groupCache.GetAll() {
		groups = append(groups, group)
	}
	return groups
}

// GroupListModel 返回缓存中的全部分组模型名。
func GroupListModel() []string {
	models := []string{}
	for _, group := range groupCache.GetAll() {
		models = append(models, group.Name)
	}
	return models
}

// GroupGetByName 返回客户端模型名称对应的完整分组配置。
func GroupGetByName(name string) (model.Group, error) {
	groupID, ok := groupNameIndex.Get(name)
	if !ok {
		return model.Group{}, fmt.Errorf("group not found")
	}
	group, ok := groupCache.Get(groupID)
	if !ok {
		return model.Group{}, fmt.Errorf("group not found")
	}
	return group, nil
}

// GroupCreate 创建分组并刷新名称和主键缓存。
func GroupCreate(group *model.Group, ctx context.Context) error {
	if group == nil {
		return fmt.Errorf("group is required")
	}
	group.ID = 0
	group.Name = strings.TrimSpace(group.Name)
	if group.Name == "" {
		return fmt.Errorf("group name is required")
	}
	group.ActiveItemID = 0
	if group.Mode == "" {
		group.Mode = model.GroupModeManual
	}
	model.NormalizeGroupRelayConfig(&group.RelayConfig)
	for i := range group.Items {
		group.Items[i].ID = 0
		group.Items[i].GroupID = 0
	}
	if err := normalizeAndValidateGroupItems(group.Items); err != nil {
		return err
	}
	if err := db.GetDB().WithContext(ctx).Create(group).Error; err != nil {
		return err
	}
	sort.Slice(group.Items, func(i, j int) bool {
		if group.Items[i].Priority != group.Items[j].Priority {
			return group.Items[i].Priority < group.Items[j].Priority
		}
		return group.Items[i].ID < group.Items[j].ID
	})
	groupCache.Set(group.ID, *group)
	groupNameIndex.Set(group.Name, group.ID)
	return nil
}

// GroupUpdate 更新分组配置和成员，并返回刷新后的分组。
func GroupUpdate(req *model.GroupUpdateRequest, ctx context.Context) (*model.Group, error) {
	oldGroup, ok := groupCache.Get(req.ID)
	if !ok {
		return nil, fmt.Errorf("group not found")
	}
	oldName := oldGroup.Name

	var selectFields []string
	updates := model.Group{ID: req.ID}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return nil, fmt.Errorf("group name is required")
		}
		selectFields = append(selectFields, "name")
		updates.Name = name
	}
	if req.Mode != nil {
		selectFields = append(selectFields, "mode")
		updates.Mode = *req.Mode
	}
	if req.RelayConfig != nil {
		config := *req.RelayConfig
		model.NormalizeGroupRelayConfig(&config)
		selectFields = append(selectFields, "relay_config")
		updates.RelayConfig = config
	}

	newItems := make([]model.GroupItem, len(req.ItemsToAdd))
	for i, item := range req.ItemsToAdd {
		newItems[i] = model.GroupItem{
			GroupID:   req.ID,
			ChannelID: item.ChannelID,
			ModelName: item.ModelName,
			Priority:  item.Priority,
		}
	}
	if err := normalizeAndValidateGroupItems(newItems); err != nil {
		return nil, err
	}

	var deletedItemIDs []int
	var group model.Group
	if err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if len(selectFields) > 0 {
			if err := tx.Model(&model.Group{}).Where("id = ?", req.ID).Select(selectFields).Updates(&updates).Error; err != nil {
				return fmt.Errorf("failed to update group: %w", err)
			}
		}

		// 删除 items
		if len(req.ItemsToDelete) > 0 {
			if err := tx.Model(&model.GroupItem{}).
				Where("id IN ? AND group_id = ?", req.ItemsToDelete, req.ID).
				Pluck("id", &deletedItemIDs).Error; err != nil {
				return fmt.Errorf("failed to find deleted items: %w", err)
			}
			if len(deletedItemIDs) > 0 {
				if err := tx.Model(&model.Group{}).
					Where("id = ? AND active_item_id IN ?", req.ID, deletedItemIDs).
					Update("active_item_id", 0).Error; err != nil {
					return fmt.Errorf("failed to clear active item: %w", err)
				}
				if err := tx.Where("id IN ?", deletedItemIDs).Delete(&model.GroupItem{}).Error; err != nil {
					return fmt.Errorf("failed to delete items: %w", err)
				}
			}
		}

		// 批量更新 items
		if len(req.ItemsToUpdate) > 0 {
			ids := make([]int, len(req.ItemsToUpdate))
			priorityCase := "CASE id"
			for i, item := range req.ItemsToUpdate {
				ids[i] = item.ID
				priorityCase += fmt.Sprintf(" WHEN %d THEN %d", item.ID, item.Priority)
			}
			priorityCase += " END"

			if err := tx.Model(&model.GroupItem{}).
				Where("id IN ? AND group_id = ?", ids, req.ID).
				Updates(map[string]interface{}{
					"priority": gorm.Expr(priorityCase),
				}).Error; err != nil {
				return fmt.Errorf("failed to update items: %w", err)
			}
		}

		// 批量新增 items
		if len(newItems) > 0 {
			if err := tx.Create(&newItems).Error; err != nil {
				return fmt.Errorf("failed to create items: %w", err)
			}
		}

		if err := tx.Preload("Items", func(tx *gorm.DB) *gorm.DB {
			return tx.Order("priority ASC").Order("id ASC")
		}).First(&group, req.ID).Error; err != nil {
			return fmt.Errorf("failed to load updated group: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	groupCache.Set(group.ID, group)
	groupNameIndex.Set(group.Name, group.ID)
	if oldName != group.Name {
		groupNameIndex.Del(oldName)
	}
	return &group, nil
}

// GroupActiveItemUpdate 更新或清空分组当前手动指定的渠道模型。
func GroupActiveItemUpdate(groupID int, req *model.GroupActiveItemUpdateRequest, ctx context.Context) (*model.Group, error) {
	group, ok := groupCache.Get(groupID)
	if !ok {
		return nil, fmt.Errorf("group not found")
	}
	itemID := *req.ItemID
	if itemID != 0 {
		found := false
		for _, item := range group.Items {
			if item.ID == itemID {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("group item not found")
		}
	}
	if err := db.GetDB().WithContext(ctx).Model(&model.Group{}).Where("id = ?", groupID).Update("active_item_id", itemID).Error; err != nil {
		return nil, fmt.Errorf("failed to update active item: %w", err)
	}
	group.ActiveItemID = itemID
	groupCache.Set(group.ID, group)
	return &group, nil
}

// GroupDel 删除分组及其分组成员。
func GroupDel(id int, ctx context.Context) error {
	group, ok := groupCache.Get(id)
	if !ok {
		return fmt.Errorf("group not found")
	}

	if err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("group_id = ?", id).Delete(&model.GroupItem{}).Error; err != nil {
			return fmt.Errorf("failed to delete group items: %w", err)
		}

		if err := tx.Delete(&model.Group{}, id).Error; err != nil {
			return fmt.Errorf("failed to delete group: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	groupCache.Del(id)
	groupNameIndex.Del(group.Name)
	return nil
}

// normalizeAndValidateGroupItems 规范化分组成员模型名，并验证引用的渠道模型真实存在。
func normalizeAndValidateGroupItems(items []model.GroupItem) error {
	for i := range items {
		items[i].ModelName = strings.TrimSpace(items[i].ModelName)
		item := items[i]
		if item.ModelName == "" {
			return fmt.Errorf("group item model name is required")
		}
		channel, err := ChannelGet(item.ChannelID)
		if err != nil {
			return fmt.Errorf("group item channel %d not found", item.ChannelID)
		}
		found := false
		for _, modelName := range xstrings.SplitCompact(",", channel.Model, channel.CustomModel) {
			if modelName == item.ModelName {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("model %q not found in channel %d", item.ModelName, item.ChannelID)
		}
	}
	return nil
}

// groupItemCleanupByChannel 在当前事务中删除指定渠道不属于最终模型集合的分组项；modelNames 为空时删除全部分组项。
func groupItemCleanupByChannel(tx *gorm.DB, channelID int, modelNames []string) ([]int, []int, error) {
	var items []model.GroupItem
	query := tx.Select("id", "group_id").Where("channel_id = ?", channelID)
	if len(modelNames) > 0 {
		query = query.Where("model_name NOT IN ?", modelNames)
	}
	if err := query.Find(&items).Error; err != nil {
		return nil, nil, fmt.Errorf("failed to find group items: %w", err)
	}
	if len(items) == 0 {
		return nil, nil, nil
	}

	seenGroupIDs := make(map[int]struct{}, len(items))
	groupIDs := make([]int, 0, len(items))
	itemIDs := make([]int, 0, len(items))
	for _, item := range items {
		itemIDs = append(itemIDs, item.ID)
		if _, ok := seenGroupIDs[item.GroupID]; !ok {
			seenGroupIDs[item.GroupID] = struct{}{}
			groupIDs = append(groupIDs, item.GroupID)
		}
	}

	if err := tx.Model(&model.Group{}).
		Where("id IN ? AND active_item_id IN ?", groupIDs, itemIDs).
		Update("active_item_id", 0).Error; err != nil {
		return nil, nil, fmt.Errorf("failed to clear active items: %w", err)
	}
	if err := tx.Where("id IN ?", itemIDs).Delete(&model.GroupItem{}).Error; err != nil {
		return nil, nil, fmt.Errorf("failed to delete group items: %w", err)
	}
	return groupIDs, itemIDs, nil
}

// groupItemCleanupCache 从缓存副本中移除已删除的分组项。
func groupItemCleanupCache(groupIDs, itemIDs []int) {
	if len(itemIDs) == 0 {
		return
	}
	deletedItemIDs := make(map[int]struct{}, len(itemIDs))
	for _, itemID := range itemIDs {
		deletedItemIDs[itemID] = struct{}{}
	}
	for _, groupID := range groupIDs {
		group, ok := groupCache.Get(groupID)
		if !ok {
			continue
		}
		items := make([]model.GroupItem, 0, len(group.Items))
		for _, item := range group.Items {
			if _, deleted := deletedItemIDs[item.ID]; !deleted {
				items = append(items, item)
			}
		}
		group.Items = items
		if _, deleted := deletedItemIDs[group.ActiveItemID]; deleted {
			group.ActiveItemID = 0
		}
		groupCache.Set(group.ID, group)
	}
}

// groupRefreshCache 从数据库刷新完整分组缓存和名称索引。
func groupRefreshCache(ctx context.Context) error {
	groups := []model.Group{}
	if err := db.GetDB().WithContext(ctx).
		Preload("Items", func(tx *gorm.DB) *gorm.DB {
			return tx.Order("priority ASC").Order("id ASC")
		}).
		Find(&groups).Error; err != nil {
		return err
	}
	for _, group := range groups {
		groupCache.Set(group.ID, group)
		groupNameIndex.Set(group.Name, group.ID)
	}
	return nil
}
