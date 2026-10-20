package sshkeys

import (
	"context"
	"database/sql"
	"errors"

	"github.com/go-sql-driver/mysql"

	"admin-server-go/internal/store"
)

// 错误语义：仓储层只回这三类，HTTP 层据此映射状态码，避免把 SQL 细节漏给调用方。
var (
	// ErrNotFound 表示目标公钥不存在。
	ErrNotFound = errors.New("公钥不存在")
	// ErrDuplicate 表示同一指纹已存在。
	ErrDuplicate = errors.New("公钥已存在")
	// ErrLimit 表示已达到 20 把上限。
	ErrLimit = errors.New("公钥数量已达上限")
)

// maxKeys 是单用户公钥上限（需求书 §二.3）。
const maxKeys = 20

// Key 是 ssh_key 表的完整行（含公钥原文，仅服务内部使用）。
type Key struct {
	ID          int64
	Title       string
	PublicKey   string
	Fingerprint string
	KeyType     string
	Comment     string
	Enabled     bool
	CreatedAt   int64
	LastUsedAt  *int64
}

// Repository 是 ssh_key 表的读写入口。
type Repository struct {
	db *store.DB
}

// NewRepository 包装数据库句柄；db 为 nil 时每个方法都返回 ErrUnavailable。
func NewRepository(db *store.DB) *Repository { return &Repository{db: db} }

// ErrUnavailable 表示数据库不可用（启动时指标库连接失败会走到这里）。
var ErrUnavailable = errors.New("SSH 公钥库不可用")

func (r *Repository) available() error {
	if r == nil || r.db == nil || r.db.SQL == nil {
		return ErrUnavailable
	}
	return nil
}

// List 按 id 倒序返回全部公钥。时间列在 SQL 里换算成 epoch 毫秒，避免 DSN 的 parseTime 影响。
func (r *Repository) List(ctx context.Context) ([]Key, error) {
	if err := r.available(); err != nil {
		return nil, err
	}
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT id, title, public_key, fingerprint, key_type, comment, enabled,
		       CAST(UNIX_TIMESTAMP(created_at) * 1000 AS SIGNED),
		       CASE WHEN last_used_at IS NULL THEN NULL ELSE CAST(UNIX_TIMESTAMP(last_used_at) * 1000 AS SIGNED) END
		FROM ssh_key ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := make([]Key, 0, 8)
	for rows.Next() {
		var (
			key      Key
			enabled  int
			lastUsed sql.NullInt64
		)
		if err := rows.Scan(&key.ID, &key.Title, &key.PublicKey, &key.Fingerprint, &key.KeyType,
			&key.Comment, &enabled, &key.CreatedAt, &lastUsed); err != nil {
			return nil, err
		}
		key.Enabled = enabled != 0
		if lastUsed.Valid {
			v := lastUsed.Int64
			key.LastUsedAt = &v
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// Get 按 id 读取一行；不存在返回 ErrNotFound。
func (r *Repository) Get(ctx context.Context, id int64) (Key, error) {
	if err := r.available(); err != nil {
		return Key{}, err
	}
	var (
		key      Key
		enabled  int
		lastUsed sql.NullInt64
	)
	err := r.db.SQL.QueryRowContext(ctx, `
		SELECT id, title, public_key, fingerprint, key_type, comment, enabled,
		       CAST(UNIX_TIMESTAMP(created_at) * 1000 AS SIGNED),
		       CASE WHEN last_used_at IS NULL THEN NULL ELSE CAST(UNIX_TIMESTAMP(last_used_at) * 1000 AS SIGNED) END
		FROM ssh_key WHERE id = ?`, id).
		Scan(&key.ID, &key.Title, &key.PublicKey, &key.Fingerprint, &key.KeyType,
			&key.Comment, &enabled, &key.CreatedAt, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, ErrNotFound
	}
	if err != nil {
		return Key{}, err
	}
	key.Enabled = enabled != 0
	if lastUsed.Valid {
		v := lastUsed.Int64
		key.LastUsedAt = &v
	}
	return key, nil
}

// Create 写入新公钥。数量上限与指纹唯一都在写库前校验；并发下的唯一键冲突映射为 ErrDuplicate。
func (r *Repository) Create(ctx context.Context, title string, parsed *ParsedKey) (Key, error) {
	if err := r.available(); err != nil {
		return Key{}, err
	}
	var count int
	if err := r.db.SQL.QueryRowContext(ctx, "SELECT COUNT(*) FROM ssh_key").Scan(&count); err != nil {
		return Key{}, err
	}
	if count >= maxKeys {
		return Key{}, ErrLimit
	}

	res, err := r.db.SQL.ExecContext(ctx, `
		INSERT INTO ssh_key (title, public_key, fingerprint, key_type, comment, enabled, created_at)
		VALUES (?, ?, ?, ?, ?, 1, NOW())`,
		title, parsed.Line, parsed.FingerprintHex, parsed.Type, parsed.Comment)
	if err != nil {
		if isDuplicate(err) {
			return Key{}, ErrDuplicate
		}
		return Key{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Key{}, err
	}
	return r.Get(ctx, id)
}

// Update 更新标题与启用状态（调用方已做归一化与校验）。
func (r *Repository) Update(ctx context.Context, id int64, title string, enabled bool) (Key, error) {
	if err := r.available(); err != nil {
		return Key{}, err
	}
	flag := 0
	if enabled {
		flag = 1
	}
	res, err := r.db.SQL.ExecContext(ctx, "UPDATE ssh_key SET title = ?, enabled = ? WHERE id = ?", title, flag, id)
	if err != nil {
		return Key{}, err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// MySQL 在「值未变化」时也可能返回 0，需再确认行是否存在。
		if _, getErr := r.Get(ctx, id); getErr != nil {
			return Key{}, getErr
		}
	}
	return r.Get(ctx, id)
}

// Delete 删除一行；不存在返回 ErrNotFound。
func (r *Repository) Delete(ctx context.Context, id int64) error {
	if err := r.available(); err != nil {
		return err
	}
	res, err := r.db.SQL.ExecContext(ctx, "DELETE FROM ssh_key WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// EnabledLines 返回全部启用公钥的 `type base64 comment` 行（辅助命令使用）。
func (r *Repository) EnabledLines(ctx context.Context) ([]string, error) {
	if err := r.available(); err != nil {
		return nil, err
	}
	rows, err := r.db.SQL.QueryContext(ctx, "SELECT public_key FROM ssh_key WHERE enabled = 1 ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	lines := make([]string, 0, 8)
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	return lines, rows.Err()
}

// TouchLastUsed 回写最近使用时间；仅更新启用中的匹配指纹，尽力而为。
func (r *Repository) TouchLastUsed(ctx context.Context, fingerprintHex string) error {
	if err := r.available(); err != nil {
		return err
	}
	_, err := r.db.SQL.ExecContext(ctx,
		"UPDATE ssh_key SET last_used_at = NOW() WHERE fingerprint = ? AND enabled = 1", fingerprintHex)
	return err
}

// isDuplicate 识别 MySQL 1062（唯一键冲突）。
func isDuplicate(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}
