package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrNotFound — каноничная ошибка «такой записи нет». Возвращаем её,
// а не 500: API-слой превратит её в HTTP 404, а не в ошибку сервера.
var ErrNotFound = errors.New("not found")

// AddDocument создаёт запись о файле в статусе pending.
// Реальное содержимое в базу не пишется — только метаданные;
// текст живёт в чанках после пайплайна.
func (s *Store) AddDocument(ctx context.Context, name string, size int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO documents (name, size, status, created_at) VALUES (?, ?, 'pending', ?)`,
		name, size, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetStatus — изменения статуса по ходу пайплайна.
// Статусы видны в веб-интерфейсе: «эмбеддинг 47%» — это оно.
func (s *Store) SetStatus(ctx context.Context, id int64, status, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE documents SET status = ?, error = ? WHERE id = ?`, status, errMsg, id)
	return err
}

// ListDocuments — список для левой панели дашборда, новые сверху.
func (s *Store) ListDocuments(ctx context.Context) ([]Document, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.id, d.name, d.size, d.status, d.error,
		       (SELECT COUNT(*) FROM chunks c WHERE c.doc_id = d.id) AS chunks,
		       d.created_at
		FROM documents d
		ORDER BY d.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Document
	for rows.Next() {
		var d Document
		var created int64
		var errStr string
		if err := rows.Scan(&d.ID, &d.Name, &d.Size, &d.Status, &errStr, &d.Chunks, &created); err != nil {
			return nil, err
		}
		d.Error = errStr
		d.CreatedAt = time.Unix(created, 0)
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteDocument — полная уборка следа документа в одной транзакции.
// Порядок важен: сначала собираем id чанков (они же rowid в FTS),
// затем чистим FTS-индекс, сами чанки и в конце — запись документа.
// Явное удаление чанков вместо надежды на ON DELETE CASCADE: каскад
// зависит от прагмы foreign_keys конкретного соединения пула, а эта
// операция обязана быть самодостаточной.
// Всё в одной транзакции: либо удаляется всё, либо ничего.
func (s *Store) DeleteDocument(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op после Commit

	// Собираем id чанков — по ним чистим FTS.
	rows, err := tx.QueryContext(ctx, `SELECT id FROM chunks WHERE doc_id = ?`, id)
	if err != nil {
		return err
	}
	var chunkIDs []int64
	for rows.Next() {
		var cid int64
		if err := rows.Scan(&cid); err != nil {
			rows.Close()
			return err
		}
		chunkIDs = append(chunkIDs, cid)
	}
	rows.Close()

	// Contentless FTS5 допускает DELETE только с опцией contentless_delete=1
	// в схеме таблицы (см. migrate) — иначе SQLite отвечает ошибкой.
	for _, cid := range chunkIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM chunks_fts WHERE rowid = ?`, cid); err != nil {
			return err
		}
	}

	// Явно удаляем чанки с векторами.
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE doc_id = ?`, id); err != nil {
		return err
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM documents WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// GetChunkText — полный текст одного чанка по id (для цитат в ответах).
func (s *Store) GetChunkText(ctx context.Context, chunkID int64) (string, error) {
	var text string
	err := s.db.QueryRowContext(ctx, `SELECT text FROM chunks WHERE id = ?`, chunkID).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return text, err
}

// Stats — числа для шапки дашборда.
func (s *Store) Stats(ctx context.Context) (docs int, chunks int, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM documents), (SELECT COUNT(*) FROM chunks)`).Scan(&docs, &chunks)
	return
}

// AddChunk записывает чанк с вектором и добавляет его в FTS-индекс.
// Транзакция обязательна: чанк в chunks и строка в chunks_fts должны
// появиться атомарно — иначе поиск на мгновение увидит рассинхрон.
func (s *Store) AddChunk(ctx context.Context, docID, seq int64, text string, vec []float32) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO chunks (doc_id, seq, text, embedding) VALUES (?, ?, ?, ?)`,
		docID, seq, text, encodeVec(vec))
	if err != nil {
		return err
	}
	chunkID, err := res.LastInsertId()
	if err != nil {
		return err
	}
	// rowid задаём ЯВНО равным chunkID: иначе FTS сгенерирует свои rowid
	// (1, 2, 3...), и удаление по rowid = chunkID из DeleteDocument
	// вычищало бы чужие строки. Явный rowid — это «мост» между таблицами.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO chunks_fts (rowid, text, chunk_id) VALUES (?, ?, ?)`,
		chunkID, text, chunkID); err != nil {
		return err
	}
	return tx.Commit()
}
