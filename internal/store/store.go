// Package store — хранилище Recall: документы, чанки, векторы.
//
// Почему SQLite, а не векторная БД: для личных объёмов (десятки тысяч
// чанков) перебор векторов в памяти занимает миллисекунды, а SQLite
// даёт ноль инфраструктуры. Хранить векторы будем в BLOB-колонке —
// и этот файл покажет, как «векторная БД» устроена изнутри.
package store

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // драйвер-подключение: чистый Go, без CGO
)

// Store — обёртка над *sql.DB. Все запросы к базе живут здесь;
// остальной код видит только методы с бизнес-смыслом.
type Store struct {
	db *sql.DB
}

// Document — запись о загруженном файле.
// Status отражает этап пайплайна: pending → parsing → embedding → ready / error.
type Document struct {
	ID        int64
	Name      string
	Size      int64
	Status    string
	Error     string
	Chunks    int
	CreatedAt time.Time
}

// New открывает базу и создаёт схему, если её ещё нет.
func New(path string) (*Store, error) {
	// Каталог создаём заранее: sqlite не умеет mkdir.
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	// Прагмы. Это настройки SQLite, а не таблицы — применяются на соединении:
	// journal_mode=WAL — читатели не блокируют писателя (у нас api и ingest
	// работают одновременно), busy_timeout — вторая горутина подождёт до 5с,
	// а не упадёт с SQLITE_BUSY, foreign_keys — каскадное удаление чанков.
	for _, pragma := range []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA foreign_keys = ON`,
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("pragma %q: %w", pragma, err)
		}
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
-- UNINDEXED column: chunk_id хранится, но не участвует в поиске —
-- это мост от найденной строки к полному чанку.
-- content='' — «contentless» таблица: FTS хранит только индекс,
-- без дублирования текста (экономим память, зато удалять надо руками — см. DeleteDocument).
CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
	text, chunk_id UNINDEXED, content=''
);

-- Мета-таблица: размерность вектора (nomic-embed-text даёт 768).
-- Храним её явно: при смене модели индекс нужно перестраивать,
-- и проверка размерности спасёт нас от тихой порчи поиска.
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
// Для 768 измерений это ровно 3072 байта на чанк. Любая СУБД с BLOB
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
