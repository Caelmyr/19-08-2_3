// Package store 实现MySQL持久化存储
package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// Config MySQL配置
type Config struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
}

// NewDB 创建数据库连接
func NewDB(cfg Config) (*sql.DB, error) {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=True&loc=Local",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.Database)

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}

	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(time.Hour)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping mysql: %w", err)
	}

	return db, nil
}

// Document 文档实体
type Document struct {
	ID             string    `json:"id"`
	Title          string    `json:"title"`
	ContentSnapshot string   `json:"content_snapshot"`
	CurrentVersion int64     `json:"current_version"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// OperationRecord 操作日志记录
type OperationRecord struct {
	ID        int64     `json:"id"`
	DocID     string    `json:"doc_id"`
	Version   int64     `json:"version"`
	ClientID  string    `json:"client_id"`
	OpType    string    `json:"op_type"`
	Position  int       `json:"position"`
	Length    int       `json:"length"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// Store 存储接口
type Store struct {
	db *sql.DB
}

// NewStore 创建Store
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// CreateDocument 创建新文档
func (s *Store) CreateDocument(id, title string) (*Document, error) {
	content := ""
	_, err := s.db.Exec(
		"INSERT INTO documents (id, title, content_snapshot, current_version) VALUES (?, ?, ?, 0)",
		id, title, content,
	)
	if err != nil {
		return nil, fmt.Errorf("insert document: %w", err)
	}
	return s.GetDocument(id)
}

// GetDocument 获取文档
func (s *Store) GetDocument(id string) (*Document, error) {
	var doc Document
	err := s.db.QueryRow(
		"SELECT id, title, COALESCE(content_snapshot,''), current_version, created_at, updated_at FROM documents WHERE id = ?",
		id,
	).Scan(&doc.ID, &doc.Title, &doc.ContentSnapshot, &doc.CurrentVersion, &doc.CreatedAt, &doc.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get document: %w", err)
	}
	return &doc, nil
}

// ListDocuments 列出所有文档
func (s *Store) ListDocuments() ([]Document, error) {
	rows, err := s.db.Query(
		"SELECT id, title, current_version, created_at, updated_at FROM documents ORDER BY updated_at DESC",
	)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer rows.Close()

	var docs []Document
	for rows.Next() {
		var doc Document
		err := rows.Scan(&doc.ID, &doc.Title, &doc.CurrentVersion, &doc.CreatedAt, &doc.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("scan document: %w", err)
		}
		docs = append(docs, doc)
	}
	return docs, rows.Err()
}

// UpdateSnapshot 更新文档快照（在checkpoint时调用）
func (s *Store) UpdateSnapshot(id, content string, version int64) error {
	_, err := s.db.Exec(
		"UPDATE documents SET content_snapshot = ?, current_version = ?, updated_at = NOW() WHERE id = ?",
		content, version, id,
	)
	return err
}

// AppendOperation 追加操作日志（事务中使用）
func (s *Store) AppendOperation(docID, clientID, opType string, version, position, length int64, content string) error {
	_, err := s.db.Exec(
		"INSERT INTO operations (doc_id, version, client_id, op_type, position, length, content) VALUES (?, ?, ?, ?, ?, ?, ?)",
		docID, version, clientID, opType, position, length, content,
	)
	return err
}

// AppendOperationTx 在事务中追加操作日志并更新版本
func (s *Store) AppendOperationTx(tx *sql.Tx, docID, clientID, opType string, version, position, length int64, content string) error {
	_, err := tx.Exec(
		"INSERT INTO operations (doc_id, version, client_id, op_type, position, length, content) VALUES (?, ?, ?, ?, ?, ?, ?)",
		docID, version, clientID, opType, position, length, content,
	)
	if err != nil {
		return err
	}
	_, err = tx.Exec(
		"UPDATE documents SET current_version = ?, updated_at = NOW() WHERE id = ?",
		version, docID,
	)
	return err
}

// GetOperationsSince 获取自某个版本以来的所有操作（增量拉取）
func (s *Store) GetOperationsSince(docID string, sinceVersion int64) ([]OperationRecord, error) {
	rows, err := s.db.Query(
		"SELECT id, doc_id, version, client_id, op_type, position, length, COALESCE(content,''), created_at "+
			"FROM operations WHERE doc_id = ? AND version > ? ORDER BY version ASC, id ASC",
		docID, sinceVersion,
	)
	if err != nil {
		return nil, fmt.Errorf("get operations: %w", err)
	}
	defer rows.Close()

	var ops []OperationRecord
	for rows.Next() {
		var op OperationRecord
		err := rows.Scan(&op.ID, &op.DocID, &op.Version, &op.ClientID, &op.OpType, &op.Position, &op.Length, &op.Content, &op.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("scan operation: %w", err)
		}
		ops = append(ops, op)
	}
	return ops, rows.Err()
}

// GetOperationsRange 获取版本范围内的操作
func (s *Store) GetOperationsRange(docID string, fromVersion, toVersion int64) ([]OperationRecord, error) {
	rows, err := s.db.Query(
		"SELECT id, doc_id, version, client_id, op_type, position, length, COALESCE(content,''), created_at "+
			"FROM operations WHERE doc_id = ? AND version > ? AND version <= ? ORDER BY version ASC, id ASC",
		docID, fromVersion, toVersion,
	)
	if err != nil {
		return nil, fmt.Errorf("get operations range: %w", err)
	}
	defer rows.Close()

	var ops []OperationRecord
	for rows.Next() {
		var op OperationRecord
		err := rows.Scan(&op.ID, &op.DocID, &op.Version, &op.ClientID, &op.OpType, &op.Position, &op.Length, &op.Content, &op.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("scan operation: %w", err)
		}
		ops = append(ops, op)
	}
	return ops, rows.Err()
}

// GetDocumentVersion 获取文档的当前版本
func (s *Store) GetDocumentVersion(docID string) (int64, error) {
	var version int64
	err := s.db.QueryRow("SELECT current_version FROM documents WHERE id = ?", docID).Scan(&version)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return version, err
}

// SaveSnapshotTransaction 在事务中保存快照
func (s *Store) SaveSnapshotTransaction(docID, content string, version int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		"UPDATE documents SET content_snapshot = ?, current_version = ?, updated_at = NOW() WHERE id = ?",
		content, version, docID,
	)
	if err != nil {
		return err
	}

	return tx.Commit()
}
