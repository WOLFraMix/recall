// Package store — хранилище Recall: документы, чанки, векторы.
//
// Почему SQLite, а не векторная БД: для личных объёмов (десятки тысяч
// чанков) перебор векторов в памяти занимает миллисекунды, а SQLite
// даёт ноль инфраструктуры. Хранить векторы будем в BLOB-колонке —
// и этот файл покажет, как «векторная БД» устроена изнутри.
package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // драйвер-подключение: чистый Go, без CGO
)

// ErrNotFound — «такой записи нет». API переводит его в честный 404,
// а не в невнятный 500: caller различает «сломалось» и «не существует».
var ErrNotFound = errors.New("not found")

// Store — обёртка над *sql.DB. Все запросы к базе живут здесь;
// остальной код видит только методы с бизнес-смыслом.
type Store struct {
	db *sql.DB
}

// Document — запись о загруженном файле.
// json-теги — это контракт API: фронтенд читает поля в нижнем регистре.
// Без тегов Go отдал бы поля как есть ("Name", "Status") — и JS увидел бы
// undefined. Явные теги = явный контракт, не зависящий от имён в Go.
type Document struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	Status    string    `json:"status"`
	Error     string    `json:"error"`
	Chunks    int       `json:"chunks"`
	CreatedAt time.Time `json:"created_at"`
}

// New открывает базу и создаёт схему, если её ещё нет.
func New(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}

	// Прагмы в DSN — ключевая деталь: они применяются К КАЖДОМУ соединению
	// пула, а не только к первому (db.Exec применил бы прагму лишь к одному
	// соединению из пула — на остальных настройки не действовали бы).
	//   journal_mode=WAL — читатели не блокируют писателя;
	//   busy_timeout=5000 — вторая горутина подождёт до 5с вместо SQLITE_BUSY;
	//   foreign_keys=1 — целостность связей документов и чанков.
	// Формат modernc-драйвера: ?_pragma=имя(значение)&_pragma=...
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// migrate создаёт таблицы. Идемпотентно: IF NOT EXISTS позволяет звать
// её при каждом старте — на существующей базе это no-op.
func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS documents (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL,
	size       INTEGER NOT NULL DEFAULT 0,
	status     TEXT NOT NULL DEFAULT 'pending',
	error      TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS chunks (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	doc_id    INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
	seq       INTEGER NOT NULL,          -- номер чанка внутри документа
	text      TEXT NOT NULL,
	embedding BLOB NOT NULL              -- вектор: float32[] в little-endian
);
CREATE INDEX IF NOT EXISTS idx_chunks_doc ON chunks(doc_id, seq);

-- FTS5 — встроенный в SQLite полнотекстовый поиск.
-- content='' — «contentless» таблица: FTS хранит только индекс,
-- без дублирования текста (экономим память).
-- contentless_delete=1 — ОБЯЗАТЕЛЬНА: без неё contentless-таблица
-- не поддерживает DELETE вовсе (SQLite отвечает ошибкой).
--
-- ВАЖНЫЙ УРОК: contentless-таблица хранит ТОЛЬКО rowid и индексируемые
-- колонки. UNINDEXED-колонка chunk_id НЕ хранится и при чтении всегда
-- NULL — прежний JOIN chunks_fts.chunk_id = chunks.id молча возвращал
-- ноль строк, и FTS-канал поиска был мёртв с первого дня. Решение:
-- не хранить ссылку отдельной колонкой, а писать в FTS с rowid чанка —
-- rowid в contentless-таблице есть всегда, и JOIN идёт по f.rowid.
CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
	text, content='', contentless_delete=1
);

-- Мета-таблица: имя модели эмбеддингов и размерность вектора
-- (bge-m3 даёт 1024, nomic-embed-text — 768). Храним их явно:
-- при смене модели индекс нужно перестраивать, и проверка меты
-- спасёт нас от тихой порчи поиска векторами чужой размерности.
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);`
	_, err := s.db.Exec(schema)
	return err
}

// Close закрывает соединение — вызывается при graceful shutdown.
func (s *Store) Close() error { return s.db.Close() }

// --- Кодировка векторов ---
//
// Вектор []float32 храним как байты little-endian: 4 байта на координату.
// Для 1024 измерений это ровно 4096 байт на чанк. Любая СУБД с BLOB
// становится «векторной базой» — вопрос только в формате и функции близости.

func encodeVec(v []float32) []byte {
	out := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(x))
	}
	return out
}

func decodeVec(b []byte) []float32 {
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}

// cosine — косинусная близость: скалярное произведение, нормированное
// на длины векторов. 1.0 = одинаковое направление (одинаковый смысл),
// 0.0 = ортогональны (не связаны). Модели-эмбеддеры нормируют векторы
// сами, поэтому знаменатель почти всегда 1 — но считать его честно,
// код не должен полагаться на чужие гарантии.
func cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0 // несовместимые размерности — не сравниваем
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// --- Документы ---

// AddDocument регистрирует документ в статусе pending: обработка
// (парсинг, эмбеддинги) пойдёт позже в фоне — это контракт пайплайна.
func (s *Store) AddDocument(ctx context.Context, name string, size int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO documents (name, size, status, created_at)
		VALUES (?, ?, 'pending', ?)`,
		name, size, time.Now().Unix(),
	)
	if err != nil {
		return 0, fmt.Errorf("insert document: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last insert id: %w", err)
	}
	return id, nil
}

// SetStatus — единая точка смены статуса документа: дашборд в UI
// читает его из ListDocuments, поэтому любой шаг пайплайна здесь виден.
// errorText пишется только при ошибке; при успешном шаге передаём "".
func (s *Store) SetStatus(ctx context.Context, docID int64, status, errorText string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE documents SET status = ?, error = ? WHERE id = ?`,
		status, errorText, docID,
	)
	return err
}

// ListDocuments — весь список для сайдбара, с числом чанков каждого
// документа (подзапросом: N+1 не нужен, SQLite сделает всё одним проходом).
func (s *Store) ListDocuments(ctx context.Context) ([]Document, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.id, d.name, d.size, d.status, d.error,
		       (SELECT COUNT(*) FROM chunks c WHERE c.doc_id = d.id),
		       d.created_at
		FROM documents d
		ORDER BY d.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Document
	for rows.Next() {
		var d Document
		var created int64
		if err := rows.Scan(&d.ID, &d.Name, &d.Size, &d.Status, &d.Error, &d.Chunks, &created); err != nil {
			return nil, err
		}
		d.CreatedAt = time.Unix(created, 0)
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteDocument удаляет документ вместе с его чанками И строками
// FTS-индекса. Порядок критичен:
//  1. сначала собираем id чанков и чистим FTS по их rowid — после
//     CASCADE строки chunks исчезнут, и связать FTS с чанками
//     будет нечем: получим «осиротевший» поисковый индекс;
//  2. затем удаляем документ — chunks уйдут каскадом.
//
// Всё в одной транзакции: либо оба шага, либо ни один.
func (s *Store) DeleteDocument(ctx context.Context, docID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op после Commit

	// Собираем id чанков документа — это будущие rowid в FTS.
	rows, err := tx.QueryContext(ctx, `SELECT id FROM chunks WHERE doc_id = ?`, docID)
	if err != nil {
		return err
	}
	var chunkIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		chunkIDs = append(chunkIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	// Чистим FTS-индекс по rowid. Отдельным запросом на каждый id:
	// чанков у одного документа — сотни, а транзакция всё равно одна.
	for _, id := range chunkIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM chunks_fts WHERE rowid = ?`, id); err != nil {
			return fmt.Errorf("delete fts row: %w", err)
		}
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM documents WHERE id = ?`, docID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound // документ не существовал — честный 404 наверху
	}
	return tx.Commit()
}

// --- Чанки и векторы ---

// AddChunk сохраняет чанк и его вектор + регистрирует чанк в FTS-индексе.
// Всё в одной транзакции: чанк без индекса — это «невидимый» для
// полнотекстового поиска кусок текста, тихая порча данных.
//
// Ключевая деталь FTS-вставки: (rowid, text) — rowid равен id чанка.
// Именно по нему KeywordSearch джойнится обратно к полному чанку:
// contentless-таблица НЕ хранит unindexed-колонок, зато rowid есть всегда.
func (s *Store) AddChunk(ctx context.Context, docID, seq int64, text string, vec []float32) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO chunks (doc_id, seq, text, embedding)
		VALUES (?, ?, ?, ?)`,
		docID, seq, text, encodeVec(vec),
	)
	if err != nil {
		return fmt.Errorf("insert chunk: %w", err)
	}
	chunkID, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("chunk last insert id: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO chunks_fts (rowid, text) VALUES (?, ?)`,
		chunkID, text,
	); err != nil {
		return fmt.Errorf("insert fts row: %w", err)
	}

	return tx.Commit()
}

// --- Статистика ---

// Stats — пара чисел для шапки интерфейса: документов и чанков.
func (s *Store) Stats(ctx context.Context) (docs, chunks int, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM documents),
		       (SELECT COUNT(*) FROM chunks)`).Scan(&docs, &chunks)
	return
}
