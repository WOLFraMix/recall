package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Summaries — статус и результат выжимки документа.
// Отдельная таблица, а не колонка в documents: выжимка — независимая
// сущность со своим жизненным циклом, и не обязана утяжелять основной
// листинг документов.
func (s *Store) InitSummary(ctx context.Context, docID int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO summaries (doc_id, status, updated_at)
		VALUES (?, 'running', ?)
		ON CONFLICT(doc_id) DO UPDATE
		SET status='running', text='', error='', updated_at=?`,
		docID, time.Now().Unix(), time.Now().Unix())
	return err
}

func (s *Store) SetSummary(ctx context.Context, docID int64, status, text, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO summaries (doc_id, status, text, error, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(doc_id) DO UPDATE
		SET status=excluded.status, text=excluded.text,
		    error=excluded.error, updated_at=excluded.updated_at`,
		docID, status, text, errMsg, time.Now().Unix())
	return err
}

// GetSummary: статус 'none' — выжимку ещё не запускали.
func (s *Store) GetSummary(ctx context.Context, docID int64) (status, text string, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT status, text FROM summaries WHERE doc_id = ?`, docID,
	).Scan(&status, &text)
	if errors.Is(err, sql.ErrNoRows) {
		return "none", "", nil
	}
	return
}

// DocChunks — текст чанков документа в порядке следования: сырьё для map-этапа.
func (s *Store) DocChunks(ctx context.Context, docID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT text FROM chunks WHERE doc_id = ? ORDER BY seq`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DocName — имя документа по id (для заголовка выжимки и имени файла).
func (s *Store) DocName(ctx context.Context, docID int64) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx,
		`SELECT name FROM documents WHERE id = ?`, docID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return name, err
}
