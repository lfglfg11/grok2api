package relational

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

// seedRuntimeBuildClient 写入一行运行时设置，模拟"老部署里落库的 Build 客户端版本"。
func seedRuntimeBuildClient(t *testing.T, database *Database, clientVersion, userAgent string) {
	t.Helper()
	payload := runtimeSettingsPayload{Config: settingsdomain.Config{}}
	payload.Config.ProviderBuild.ClientVersion = clientVersion
	payload.Config.ProviderBuild.UserAgent = userAgent
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	row := runtimeSettingsModel{Key: runtimeSettingsKey, ValueJSON: string(encoded), Revision: 1}
	if err := database.db.Create(&row).Error; err != nil {
		t.Fatalf("seed runtime settings: %v", err)
	}
}

func readRuntimeBuildClient(t *testing.T, database *Database) (string, string) {
	t.Helper()
	var row runtimeSettingsModel
	if err := database.db.Where("key = ?", runtimeSettingsKey).First(&row).Error; err != nil {
		t.Fatalf("load runtime settings: %v", err)
	}
	var payload runtimeSettingsPayload
	if err := json.Unmarshal([]byte(row.ValueJSON), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return payload.Config.ProviderBuild.ClientVersion, payload.Config.ProviderBuild.UserAgent
}

// TestMigrateBuildClientVersionUpgradesSupersededDefault 复现生产事故：
// 库里留着 0.2.119（曾是代码默认值），上游把默认值抬到 1.0.40 后，老部署仍然
// 以 0.2.119 请求上游，所有 Build 模型被 426 拒绝。迁移必须把它抬到当前推荐值。
func TestMigrateBuildClientVersionUpgradesSupersededDefault(t *testing.T) {
	if config.RecommendedBuildClientVersion == "0.2.119" {
		t.Skip("推荐版本已经是 0.2.119，该用例不再有区分度")
	}
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("初始化 schema: %v", err)
	}
	seedRuntimeBuildClient(t, database, "0.2.119", "grok-shell/0.2.119 (linux; x86_64)")

	if err := database.migrateBuildClientVersion(ctx); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	version, userAgent := readRuntimeBuildClient(t, database)
	if version != config.RecommendedBuildClientVersion {
		t.Fatalf("clientVersion = %q，期望 %q", version, config.RecommendedBuildClientVersion)
	}
	if userAgent != config.RecommendedBuildUserAgent {
		t.Fatalf("userAgent = %q，期望 %q", userAgent, config.RecommendedBuildUserAgent)
	}
}

// TestMigrateBuildClientVersionKeepsCustomPin 确认人工固定的版本不会被迁移覆盖，
// 只要 userAgent 不是由该版本派生的默认 UA。
func TestMigrateBuildClientVersionKeepsCustomPin(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct{ version, userAgent string }{
		"自定义 UA 的同名版本": {"0.2.119", "my-custom-agent/9.9"},
		"从未作为默认值的版本":   {"0.2.88", "grok-shell/0.2.88 (linux; x86_64)"},
		"已经是当前推荐值":     {config.RecommendedBuildClientVersion, config.RecommendedBuildUserAgent},
		"版本为空由代码默认值兜底": {"", "grok-shell/0.2.119 (linux; x86_64)"},
	} {
		t.Run(name, func(t *testing.T) {
			database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "settings.db"))
			if err != nil {
				t.Fatalf("打开数据库: %v", err)
			}
			defer database.Close()
			if err := database.InitializeSchema(ctx); err != nil {
				t.Fatalf("初始化 schema: %v", err)
			}
			seedRuntimeBuildClient(t, database, tc.version, tc.userAgent)

			if err := database.migrateBuildClientVersion(ctx); err != nil {
				t.Fatalf("迁移: %v", err)
			}
			version, userAgent := readRuntimeBuildClient(t, database)
			if version != tc.version || userAgent != tc.userAgent {
				t.Fatalf("人工值被改写: clientVersion=%q userAgent=%q", version, userAgent)
			}
		})
	}
}

// TestMigrateBuildClientVersionIsNoopWithoutSettingsRow 确认库里还没有设置行时不报错。
func TestMigrateBuildClientVersionIsNoopWithoutSettingsRow(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("初始化 schema: %v", err)
	}
	if err := database.migrateBuildClientVersion(ctx); err != nil {
		t.Fatalf("缺少设置行时不应报错: %v", err)
	}
}
