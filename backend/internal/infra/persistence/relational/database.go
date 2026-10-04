package relational

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	glebarezsqlite "github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Database 持有关系型数据库连接和各仓储实现共享的 GORM 实例。
type Database struct {
	db      *gorm.DB
	dialect string
}

func (d *Database) Stats() sql.DBStats {
	if d == nil {
		return sql.DBStats{}
	}
	sqlDB, err := d.db.DB()
	if err != nil {
		return sql.DBStats{}
	}
	return sqlDB.Stats()
}

func (d *Database) Dialect() string {
	if d == nil {
		return ""
	}
	return d.dialect
}

// OpenSQLite 打开纯 Go SQLite 数据库并启用 WAL、外键与 busy timeout。
// 显式事务使用 IMMEDIATE，避免并发读后写事务在锁升级时直接返回 SQLITE_BUSY。
//
// 两点顺序与取值上的约束（改动前务必确认，否则会静默失效）：
//  1. auto_vacuum 必须排在 journal_mode 之前。journal_mode=WAL 会写入库头，
//     一旦库头落地，auto_vacuum 就只能靠 VACUUM 生效——DSN 里的设置对新建库也会
//     变成空操作。对已存在的库该 pragma 无害但也无效，需要一次性 VACUUM 转换；
//     启用后由周期任务调用 PRAGMA incremental_vacuum 把空闲页真正还给文件系统。
//  2. busy_timeout 必须显著大于业务写入上下文的超时。启动阶段大量后台写入
//     （凭据刷新、配额回补、失效标记）会长时间占住 SQLite 唯一的写锁；
//     原先 5s 与 credentialStateWriteTimeout 的 5s 相等，两者互相抢跑，
//     结果是刷新令牌轮换后的落库被丢弃，账号被迫重新认证。
func OpenSQLite(ctx context.Context, path string) (*Database, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("创建数据库目录: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=auto_vacuum(2)&_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate", path)
	db, err := gorm.Open(glebarezsqlite.Open(dsn), gormConfig())
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite: %w", err)
	}
	return configureDatabase(ctx, db, "sqlite", 16, 16)
}

// OpenPostgres 打开 PostgreSQL 数据库并配置连接池。
func OpenPostgres(ctx context.Context, dsn string, maxOpenConns, maxIdleConns int) (*Database, error) {
	db, err := gorm.Open(postgres.Open(dsn), gormConfig())
	if err != nil {
		return nil, &postgresConnectionError{operation: "打开 PostgreSQL", err: err, dsn: dsn}
	}
	database, err := configureDatabase(ctx, db, "postgres", maxOpenConns, maxIdleConns)
	if err != nil {
		return nil, &postgresConnectionError{operation: "配置 PostgreSQL", err: err, dsn: dsn}
	}
	return database, nil
}

type postgresConnectionError struct {
	operation string
	err       error
	dsn       string
}

func (e *postgresConnectionError) Error() string {
	return e.operation + ": " + redactPostgresErrorMessage(e.err, e.dsn)
}

func (e *postgresConnectionError) Unwrap() error { return e.err }

var (
	postgresURLPasswordPattern = regexp.MustCompile(`(?i)(postgres(?:ql)?://[^:/\s]+:)[^@\s]+(@)`)
	postgresDSNPasswordPattern = regexp.MustCompile(`(?i)(password\s*=\s*)(?:'[^']*'|"[^"]*"|[^\s]+)`)
)

func redactPostgresErrorMessage(err error, dsn string) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if value := strings.TrimSpace(dsn); value != "" {
		message = strings.ReplaceAll(message, value, "<redacted PostgreSQL DSN>")
	}
	message = postgresURLPasswordPattern.ReplaceAllString(message, `${1}<redacted>${2}`)
	return postgresDSNPasswordPattern.ReplaceAllString(message, `${1}<redacted>`)
}

func gormConfig() *gorm.Config {
	return &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Silent),
		TranslateError: true,
		NowFunc:        func() time.Time { return time.Now().UTC() },
	}
}

func configureDatabase(ctx context.Context, db *gorm.DB, dialect string, maxOpenConns, maxIdleConns int) (*Database, error) {
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(maxOpenConns)
	sqlDB.SetMaxIdleConns(maxIdleConns)
	sqlDB.SetConnMaxLifetime(time.Hour)
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("连接 %s: %w", dialect, err)
	}
	return &Database{db: db, dialect: dialect}, nil
}

// Close 关闭底层数据库连接。
func (d *Database) Close() error {
	sqlDB, err := d.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
