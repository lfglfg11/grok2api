package relational

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

// TestAuditRepositoryTrimFreePagesReclaimsFile 覆盖生产库膨胀到 677MB、其中 364MB
// 全是死页的根因：SQLite 的 DELETE 只把页挂回 freelist，文件不会自己变小。
// 开启 incremental auto-vacuum 后，必须由 TrimFreePages 才会真正回收。
func TestAuditRepositoryTrimFreePagesReclaimsFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "trim.db")
	database, err := OpenSQLite(ctx, path)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	defer database.Close()

	// DSN 里 auto_vacuum 必须排在 journal_mode 之前，否则新建库也拿不到 INCREMENTAL。
	var mode int
	if err := database.db.Raw("PRAGMA auto_vacuum").Scan(&mode).Error; err != nil {
		t.Fatalf("读取 auto_vacuum: %v", err)
	}
	if mode != 2 {
		t.Fatalf("auto_vacuum = %d，期望 2（INCREMENTAL）；DSN 中 auto_vacuum 的顺序可能被改回去了", mode)
	}

	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("初始化 schema: %v", err)
	}
	audits := NewAuditRepository(database)

	now := time.Now().UTC()
	for i := 0; i < 400; i++ {
		record := audit.Record{
			RequestID:   "req-trim-" + itoa(i),
			ClientKeyID: 1, ModelRouteID: 1,
			Provider: "grok_web", Operation: "chat", UsageSource: "upstream",
			StatusCode: 200,
			// 全部落在保留期外，随后的清理会一次删光。
			CreatedAt: now.Add(-30 * 24 * time.Hour),
		}
		if err := audits.Create(ctx, record); err != nil {
			t.Fatalf("写入审计 %d: %v", i, err)
		}
	}
	if err := database.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error; err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	sizeBefore := fileSize(t, path)

	deleted, err := audits.PurgeOlderThan(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("清理过期审计: %v", err)
	}
	if deleted == 0 {
		t.Fatalf("没有清理到任何记录，用例前置条件不成立")
	}
	var free int
	if err := database.db.Raw("PRAGMA freelist_count").Scan(&free).Error; err != nil {
		t.Fatalf("读取 freelist_count: %v", err)
	}
	if free == 0 {
		t.Fatalf("清理后 freelist_count = 0，无法验证回收")
	}

	if err := audits.TrimFreePages(ctx); err != nil {
		t.Fatalf("回收空闲页: %v", err)
	}
	if err := database.db.Raw("PRAGMA freelist_count").Scan(&free).Error; err != nil {
		t.Fatalf("读取 freelist_count: %v", err)
	}
	if free != 0 {
		t.Fatalf("回收后 freelist_count = %d，期望 0", free)
	}
	if err := database.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error; err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if sizeAfter := fileSize(t, path); sizeAfter >= sizeBefore {
		t.Fatalf("回收后文件未缩小: before=%d after=%d", sizeBefore, sizeAfter)
	}
}

// TestAuditRepositoryTrimFreePagesSkipsNonIncremental 确认未启用 incremental
// auto-vacuum 时回收是安全的空操作，老库与 PostgreSQL 部署都不会因此报错。
func TestAuditRepositoryTrimFreePagesSkipsNonIncremental(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "noop.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	defer database.Close()

	// 模拟未启用 incremental auto-vacuum 的库。
	if err := database.db.Exec("PRAGMA auto_vacuum(0)").Error; err != nil {
		t.Fatalf("关闭 auto_vacuum: %v", err)
	}
	if err := NewAuditRepository(database).TrimFreePages(ctx); err != nil {
		t.Fatalf("未启用 incremental auto-vacuum 时不应报错: %v", err)
	}

	// 模拟其他方言：只有 sqlite 才执行回收。
	postgres := &Database{db: database.db, dialect: "postgres"}
	if err := NewAuditRepository(postgres).TrimFreePages(ctx); err != nil {
		t.Fatalf("非 SQLite 方言时不应报错: %v", err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 8)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
