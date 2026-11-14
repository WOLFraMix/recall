// cmd/ftscheck — прямой осмотр FTS-канала: без KeywordSearch,
// без RRF, без HTTP. Отвечает на три вопроса:
//  1. есть ли вообще строки в FTS-индексе;
//  2. находят ли их rowid'ы свои чанки (или база «старая»);
//  3. что возвращает MATCH для слов пользователя.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/WOLFraMix/recall/internal/store"
	_ "modernc.org/sqlite"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Println("usage: go run ./cmd/ftscheck <db-путь> <слова запроса...>")
		fmt.Println("пример: go run ./cmd/ftscheck ./data/recall.db на машине")
		os.Exit(2)
	}
	dbPath := args[0]
	words := strings.Join(args[1:], " ")

	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	defer db.Close()

	var nChunks, nFts, joined int
	if err := db.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&nChunks); err != nil {
		fmt.Println("chunks count:", err)
		os.Exit(1)
	}
	_ = db.QueryRow(`SELECT COUNT(*) FROM chunks_fts`).Scan(&nFts)
	_ = db.QueryRow(`SELECT COUNT(*) FROM chunks_fts f JOIN chunks c ON c.id = f.rowid`).Scan(&joined)

	fmt.Printf("чанков в chunks:           %d\n", nChunks)
	fmt.Printf("строк в chunks_fts:        %d\n", nFts)
	fmt.Printf("fts-строк с валидным rowid: %d\n\n", joined)

	if nFts == 0 {
		fmt.Println("→ FTS-индекс ПУСТ: вставки в chunks_fts не выполняются.")
		fmt.Println("  Проверьте, что в AddChunk есть INSERT INTO chunks_fts(rowid, text),")
		fmt.Println("  и что база пересоздана, а документы загружены заново.")
		return
	}
	if joined < nFts {
		fmt.Printf("→ %d FTS-строк НЕ находят свой чанк по rowid — база старая.\n", nFts-joined)
		fmt.Println("  Остановите приложение и удалите data/recall.db* (все три файла),")
		fmt.Println("  затем запустите заново и перезалейте документы.")
		return
	}
	fmt.Println("→ FTS-индекс цел, rowid-связка работает.")

	if words == "" {
		return
	}

	// Демо выдачи — через продакшен-логику (стоп-слова, DF-фильтр,
	// OR-сборка): инструмент показывает то же, что увидит чат.
	st, err := store.New(dbPath)
	if err != nil {
		fmt.Println("store:", err)
		return
	}
	defer st.Close()

	hits, err := st.KeywordSearch(context.Background(), words, 10)
	if err != nil {
		fmt.Println("KeywordSearch:", err)
		return
	}
	fmt.Println("\nвыдача KeywordSearch (продакшен-логика):")
	for i, h := range hits {
		text := h.Text
		if len(text) > 100 {
			text = text[:100] + "…"
		}
		fmt.Printf("%2d. %s · фрагм. %d — %s\n", i+1, h.DocName, h.Seq+1, text)
	}
	if len(hits) == 0 {
		fmt.Println("пусто — все термы отсечены фильтрами или не нашлись")
	}
}
