// cmd/ftscheck — прямой осмотр FTS-канала: без KeywordSearch,
// без RRF, без HTTP. Отвечает на три вопроса:
//  1. есть ли вообще строки в FTS-индексе;
//  2. находят ли их rowid'ы свои чанки (или база «старая»);
//  3. что возвращает MATCH для слов пользователя.
package main

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

// buildMatch повторяет логику KeywordSearch: слово длиннее 4 букв
// становится префиксом (машине → машин*), короче — точной формой.
func buildMatch(query string) string {
	var terms []string
	for _, w := range strings.Fields(query) {
		r := []rune(strings.ToLower(w))
		if len(r) == 0 {
			continue
		}
		if len(r) > 4 {
			terms = append(terms, string(r[:len(r)-1])+"*")
		} else {
			terms = append(terms, `"`+strings.ReplaceAll(w, `"`, `""`)+`"`)
		}
	}
	return strings.Join(terms, " OR ")
}

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

	match := buildMatch(words)
	fmt.Println("\nMATCH-запрос:", match)
	rows, err := db.Query(`
		SELECT c.id, d.name, c.seq, c.text
		FROM chunks_fts f
		JOIN chunks c ON c.id = f.rowid
		JOIN documents d ON d.id = c.doc_id
		WHERE chunks_fts MATCH ?
		ORDER BY bm25(chunks_fts) ASC
		LIMIT 10`, match)
	if err != nil {
		fmt.Println("MATCH:", err)
		return
	}
	defer rows.Close()

	i := 0
	for rows.Next() {
		var id, seq int64
		var name, text string
		if err := rows.Scan(&id, &name, &seq, &text); err != nil {
			fmt.Println("scan:", err)
			return
		}
		i++
		if len(text) > 100 {
			text = text[:100] + "…"
		}
		fmt.Printf("%2d. %s · фрагм. %d — %s\n", i, name, seq+1, text)
	}
	if i == 0 {
		fmt.Println("MATCH не вернул ни одной строки — канал мёртв даже при живом индексе")
	}
}
