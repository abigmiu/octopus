package migrate

import (
	"fmt"

	"gorm.io/gorm"
)

func init() {
	// 必须在 AutoMigrate 之前执行：分组模型已删除，AutoMigrate 不会再感知这两张表，
	// 也不会自动删除 api_keys 上遗留的 supported_models 列。
	RegisterBeforeAutoMigration(Migration{
		Version: 9,
		Up:      migrateDropGroups,
	})
}

// migrateDropGroups 彻底移除分组功能的存量结构：
// 删除 group_items、groups 两张表，并删除 api_keys.supported_models 列（模型白名单已废弃）。
// 全部操作先判断存在性，保证重复执行幂等。
func migrateDropGroups(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}

	// 先删子表再删主表，避免外键约束阻塞。
	for _, table := range []string{"group_items", "groups"} {
		if !db.Migrator().HasTable(table) {
			continue
		}
		if err := db.Migrator().DropTable(table); err != nil {
			return fmt.Errorf("failed to drop %s: %w", table, err)
		}
	}

	// 表不存在时无需处理列（全新部署）。
	if !db.Migrator().HasTable("api_keys") {
		return nil
	}
	if err := dropColumnIfExists(db, "api_keys", "api_keys", "supported_models"); err != nil {
		return err
	}
	return nil
}
