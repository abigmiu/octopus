package migrate

import (
	"fmt"

	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
)

func init() {
	// 必须在 AutoMigrate 之前执行：AutoMigrate 既不会改主键，也不会删掉旧的 id 列。
	RegisterBeforeAutoMigration(Migration{
		Version: 8,
		Up:      migrateStatsModelKey,
	})
}

// statsModelBatchSize 控制每批写回的行数，避免超过数据库绑定参数上限。
const statsModelBatchSize = 200

// migrateStatsModelKey 将模型统计从 group_items.id 主键改为 (channel_id, name) 复合主键，
// 并把旧结构里分散在各分组的同一渠道模型统计累加成一行。
func migrateStatsModelKey(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	// 全新部署，交给后续 AutoMigrate 直接建新结构。
	if !db.Migrator().HasTable("stats_models") {
		return nil
	}
	// 没有 id 列说明已是新结构，保证幂等。
	if !db.Migrator().HasColumn("stats_models", "id") {
		return nil
	}

	type legacyStatsModel struct {
		ChannelID int    // 渠道主键。
		Name      string // 模型名。
		model.StatsMetrics
	}
	legacyRows := make([]legacyStatsModel, 0)
	if err := db.Table("stats_models").
		Select("channel_id, name, input_token, output_token, input_cost, output_cost, wait_time, request_success, request_failed").
		Find(&legacyRows).Error; err != nil {
		return fmt.Errorf("failed to read stats_models: %w", err)
	}

	// 按 (channel_id, name) 累加，同时丢弃无法归属的脏数据。
	indexByKey := make(map[string]int, len(legacyRows))
	rows := make([]model.StatsModel, 0, len(legacyRows))
	for _, legacyRow := range legacyRows {
		if legacyRow.Name == "" || legacyRow.ChannelID <= 0 {
			continue
		}
		key := fmt.Sprintf("%d:%s", legacyRow.ChannelID, legacyRow.Name)
		if i, ok := indexByKey[key]; ok {
			rows[i].StatsMetrics.Add(legacyRow.StatsMetrics)
			continue
		}
		indexByKey[key] = len(rows)
		rows = append(rows, model.StatsModel{
			ChannelID:    legacyRow.ChannelID,
			Name:         legacyRow.Name,
			StatsMetrics: legacyRow.StatsMetrics,
		})
	}

	if err := db.Migrator().DropTable("stats_models"); err != nil {
		return fmt.Errorf("failed to drop stats_models: %w", err)
	}
	// 在迁移内部建出新结构，后续全局 AutoMigrate 对该表即为 no-op。
	if err := db.AutoMigrate(&model.StatsModel{}); err != nil {
		return fmt.Errorf("failed to auto migrate stats_models: %w", err)
	}

	for start := 0; start < len(rows); start += statsModelBatchSize {
		end := start + statsModelBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		batch := rows[start:end]
		if err := db.Create(&batch).Error; err != nil {
			return fmt.Errorf("failed to write stats_models: %w", err)
		}
	}

	return nil
}
