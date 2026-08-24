package migrate

import (
	"fmt"

	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 6,
		Up:      migrateDropLegacyGroupColumns,
	})
}

// migrateDropLegacyGroupColumns 移除旧版分组模式字段，这些字段已不再使用。
// 旧版 groups 表包含 match_regex、first_token_time_out、session_keep_time，group_items 表遗留了 weight 字段。
// mode 不在清理范围内: 该列在当前模型中重新启用并承载新的选择模式，AutoMigrate 会在本迁移之前建出它，
// 删除它会让紧随其后的 007 因 groups.mode 不存在而失败，全新部署将无法启动。
func migrateDropLegacyGroupColumns(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}

	if db.Migrator().HasTable("groups") {
		legacyGroupColumns := []string{"match_regex", "first_token_time_out", "session_keep_time"}
		for _, column := range legacyGroupColumns {
			if err := dropColumnIfExists(db, &model.Group{}, "groups", column); err != nil {
				return err
			}
		}
	}

	if db.Migrator().HasTable("group_items") {
		if err := dropColumnIfExists(db, &model.GroupItem{}, "group_items", "weight"); err != nil {
			return err
		}
	}

	return nil
}
