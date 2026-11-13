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
-- UNINDEXED column: chunk_id хранится, но не участвует в поиске —
-- это мост от найденной строки к полному чанку.
-- content='' — «contentless» таблица: FTS хранит только индекс,
-- без дублирования текста (экономим память).
-- contentless_delete=1 — ОБЯЗАТЕЛЬНА: без неё contentless-таблица
-- не поддерживает DELETE вовсе (SQLite отвечает ошибкой), а нам
-- удалять строки FTS при удалении документа — см. DeleteDocument.
CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
	text, chunk_id UNINDEXED, content='', contentless_delete=1
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
