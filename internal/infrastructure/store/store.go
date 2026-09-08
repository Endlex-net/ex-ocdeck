// Package store 提供 SQLite 持久化（modernc.org/sqlite 纯 Go 驱动）。
//
// 设计要点（design.md §8）：
//   - 单 DB 连接（个人项目规模，无需连接池）
//   - 连接级 PRAGMA foreign_keys=ON、busy_timeout
//   - DB 文件权限 0600
//   - 嵌入式按版本 migration
//   - 事务短小，事务内禁止等待 git/进程/网络
//
// 本包未使用 sqlc 生成代码（本机无 sqlc CLI），改为手写与 sqlc 同结构的 Queries 层，
// 方法签名风格对齐 sqlc 生成产物（见 queries 目录下的 schema 注释）。
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DB 封装单连接 SQLite 数据库句柄，提供 Migrate 与 Queries。
type DB struct {
	*sql.DB
	*Queries
}

// Open 打开 <dataDir>/ocdeck.db 并完成连接级 PRAGMA 设置与 migration。
// 调用方负责 Close。
func Open(ctx context.Context, dataDir string) (*DB, error) {
	dbPath := filepath.Join(dataDir, "ocdeck.db")
	// 文件存在时强制 0600；新建时 modernc.org/sqlite 走 umask，下面 chmod 兜底。
	if err := ensureDBFilePerms(dbPath); err != nil {
		return nil, err
	}

	dsn := "file:" + dbPath + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}
	// 单连接：SQLite 写锁全局，多连接徒增争用与死锁面（design.md §8）。
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(0)

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping sqlite %s: %w", dbPath, err)
	}

	db := &DB{DB: sqlDB, Queries: New(sqlDB)}
	if err := db.Migrate(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}

	// migration 可能新建文件，确保 0600（design.md §8）。
	if err := chmodDBFile(dbPath); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// WithTx 在单个事务内执行 fn，传入绑定该事务的 Queries。
// MaxOpenConns(1) 下事务内 MUST 全部走 tx Queries，不得再经 d.DB 查询（自锁）。
// fn 返回 nil 提交、返回错误回滚。Commit 失败回滚并返回该错误。
func (d *DB) WithTx(ctx context.Context, fn func(q *Queries) error) error {
	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(New(tx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	committed = true
	return nil
}

// Migrate 执行嵌入式按版本 migration（design.md §8）。
// 幂等：已应用的版本跳过；未应用版本在单事务内执行。
func (d *DB) Migrate(ctx context.Context) error {
	// migration 为本地 DDL，不携带取消语义；预留 ctx 以便未来接入。
	_ = ctx
	return d.runMigrations()
}

// migrationsRequiringForeignKeysOff 必须在 begin 前于连接级关闭 FK 强制执行的
// migration 版本（事务内 PRAGMA foreign_keys 为 no-op）。0014 重建 projects 表：
// FK ON 下 DROP projects 的隐式 DELETE 会级联清空 tasks 等子表数据，故必须先
// 解除强制，commit 前用 foreign_key_check 兜底完整性
// （design allow-duplicate-project-path D3）。
var migrationsRequiringForeignKeysOff = map[int]struct{}{14: {}}

func (d *DB) runMigrations() (err error) {
	// 引导读取已应用版本（事务外、只读；全新库 schema_version 表不存在视为空集合）。
	applied, err := appliedVersions(d.DB)
	if err != nil {
		return err
	}

	names, err := migrationNames()
	if err != nil {
		return err
	}

	type pendingMigration struct {
		name string
		ver  int
	}
	var pending []pendingMigration
	for _, name := range names {
		ver, err := migrationVersion(name)
		if err != nil {
			return err
		}
		if _, ok := applied[ver]; ok {
			continue
		}
		pending = append(pending, pendingMigration{name: name, ver: ver})
	}

	// 待执行集合含 no-FK 登记版本 → begin 前连接级关闭 FK 强制；否则维持既有 ON。
	fkOff := false
	for _, m := range pending {
		if _, ok := migrationsRequiringForeignKeysOff[m.ver]; ok {
			fkOff = true
			break
		}
	}
	pragma := "PRAGMA foreign_keys=ON;"
	if fkOff {
		pragma = "PRAGMA foreign_keys=OFF;"
	}
	if _, err := d.DB.Exec(pragma); err != nil {
		return fmt.Errorf("set foreign_keys for migration: %w", err)
	}
	if fkOff {
		// commit/回滚后恢复 FK ON 并读回验证（tx 回滚 defer 先注册后执行，
		// 保证恢复发生在事务收束之后）；恢复失败返回错误、拒绝启动，
		// 与迁移失败同时发生时经 errors.Join 两者在返回错误中均可观察。
		defer func() {
			if restoreForeignKeysHook != nil {
				restoreForeignKeysHook()
			}
			if rerr := restoreForeignKeysOn(d.DB); rerr != nil {
				err = errors.Join(err, rerr)
			}
		}()
	}

	tx, err := d.DB.Begin()
	if err != nil {
		return fmt.Errorf("begin migration tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// schema_version 表由 0001_init.sql 创建；首次运行可能尚不存在，
	// 先尝试建空表保证后续读取幂等（IF NOT EXISTS）。
	if _, err := tx.Exec("CREATE TABLE IF NOT EXISTS schema_version (version INTEGER PRIMARY KEY)"); err != nil {
		return fmt.Errorf("ensure schema_version table: %w", err)
	}

	for _, m := range pending {
		content, err := migrationsFS.ReadFile(filepath.Join("migrations", m.name))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", m.name, err)
		}
		if _, err := tx.Exec(string(content)); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
		if _, err := tx.Exec("INSERT INTO schema_version (version) VALUES (?)", m.ver); err != nil {
			return fmt.Errorf("record migration %s: %w", m.name, err)
		}
	}

	// 本次关闭过 FK 时，commit 前完整性兜底：任何违规行或查询出错 → 回滚报错。
	if fkOff {
		rows, err := tx.Query("PRAGMA foreign_key_check")
		if err != nil {
			return fmt.Errorf("migration foreign_key_check: %w", err)
		}
		violated := rows.Next()
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("migration foreign_key_check: %w", err)
		}
		rows.Close()
		if violated {
			return fmt.Errorf("migration foreign_key_check reported violations")
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration tx: %w", err)
	}
	committed = true
	return nil
}

// appliedVersions 事务外引导读取已应用版本集合。schema_version 表不存在
// （全新库）视为空集合；存在性探测或版本读取失败必须返回错误，不得当作空集合。
func appliedVersions(db *sql.DB) (map[int]struct{}, error) {
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'schema_version'`).Scan(&n); err != nil {
		return nil, fmt.Errorf("probe schema_version table: %w", err)
	}
	if n == 0 {
		return map[int]struct{}{}, nil
	}
	rows, err := db.Query("SELECT version FROM schema_version")
	if err != nil {
		return nil, fmt.Errorf("load applied versions: %w", err)
	}
	defer rows.Close()
	applied := make(map[int]struct{})
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = struct{}{}
	}
	return applied, rows.Err()
}

// restoreForeignKeysOn 恢复连接级 FK 强制为 ON 并读回验证（no-FK 迁移收尾）。
// restoreForeignKeysHook 供测试在 FK 恢复阶段注入故障（如关闭 DB），随后仍执行
// 真实的 restoreForeignKeysOn；生产为 nil 不调用，零开销
// （注入模式同 queries.go beforeConditionalUpdateHook）。
var restoreForeignKeysHook func()

func restoreForeignKeysOn(db *sql.DB) error {
	if _, err := db.Exec("PRAGMA foreign_keys=ON;"); err != nil {
		return fmt.Errorf("restore foreign_keys=ON: %w", err)
	}
	var on int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&on); err != nil {
		return fmt.Errorf("verify foreign_keys: %w", err)
	}
	if on != 1 {
		return fmt.Errorf("foreign_keys not restored (got %d)", on)
	}
	return nil
}

func migrationNames() ([]string, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func migrationVersion(name string) (int, error) {
	// 形如 0001_init.sql → 1。
	base := strings.TrimSuffix(name, ".sql")
	dot := strings.IndexByte(base, '_')
	if dot < 0 {
		dot = len(base)
	}
	v, err := strconv.Atoi(base[:dot])
	if err != nil {
		return 0, fmt.Errorf("parse migration version %s: %w", name, err)
	}
	return v, nil
}

// ensureDBFilePerms 文件已存在时校验 0600。
func ensureDBFilePerms(dbPath string) error {
	if info, err := os.Stat(dbPath); err == nil {
		if info.Mode().Perm() != 0o600 {
			if err := os.Chmod(dbPath, 0o600); err != nil {
				return fmt.Errorf("chmod %s to 0600: %w", dbPath, err)
			}
		}
	}
	return nil
}

func chmodDBFile(dbPath string) error {
	if err := os.Chmod(dbPath, 0o600); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("chmod %s to 0600: %w", dbPath, err)
	}
	return nil
}
