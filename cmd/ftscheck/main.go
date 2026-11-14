// cmd/ftscheck — прямой осмотр FTS-канала.
// Отвечает на четыре вопроса:
//  1. есть ли вообще строки в FTS-индексе;
//  2. находят ли их rowid'ы свои чанки (или база «старая»);
//  3. как продакшен-логика поняла запрос: план термов, их df и решения фильтров;
//  4. что возвращает KeywordSearch с этой логикой.
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
	if len(args) < 2 {
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

	// Дальше — через store.New: тот же пакет, что у приложения, поэтому
	// план и выдача совпадают с продакшеном по определению, а не «похожи».
	st, err := store.New(dbPath)
	if err != nil {
		fmt.Println("store:", err)
		return
	}
	defer st.Close()

	// План запроса: какие термы построены, их df в корпусе и решения
	// фильтров (стоп-слова, абсолютный и относительный DF-потолки).
	// Это главная диагностика: она показывает, ЧТО канал понял из запроса.
	plan, dfMin, err := st.PlanQuery(context.Background(), words)
	if err != nil {
		fmt.Println("план:", err)
		return
	}
	fmt.Printf("\nплан запроса (специфичность: df редчайшего терма = %d):\n", dfMin)
	for _, t := range plan {
		status := "берём"
		if !t.Kept {
			status = "отсечён: " + t.Reason
		}
		fmt.Printf("  %-20s df=%-4d %s\n", t.Term, t.DF, status)
	}

	// Выдача — через продакшен-логику KeywordSearch.
	hits, err := st.KeywordSearch(context.Background(), words, 10)
	if err != nil {
		fmt.Println("KeywordSearch:", err)
		return
	}
	fmt.Println("\nвыдача KeywordSearch:")
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
