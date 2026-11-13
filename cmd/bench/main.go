// cmd/bench — измеритель качества ретривера.
//
// Прогоняет набор контрольных вопросов через настоящий поиск (база
// и конфиг — те же, что у приложения) и считает две метрики:
//
//	Hit@k  — доля вопросов, где ожидаемый фрагмент попал в топ-k;
//	MRR    — среднее 1/позиция первого правильного фрагмента.
//
// Зачем: любое изменение поиска (веса RRF, reranker, смена эмбеддера)
// проверяется цифрами «до/после», а не ощущениями. Правило гигиены:
// кейсы фиксируют то, что спрашивают РЕАЛЬНЫЕ пользователи. Их нельзя
// подгонять под текущую реализацию — иначе бенчмарк превращается
// в самолётификацию.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/WOLFraMix/recall/internal/config"
	"github.com/WOLFraMix/recall/internal/llm"
	"github.com/WOLFraMix/recall/internal/store"
)

// Case — один контрольный вопрос. Expect — подстроки: вопрос считается
// покрытым, если хотя бы одна встретилась хотя бы в одном фрагменте топ-k.
// Не задаём номера фрагментов: они плывут при изменении чанкинга,
// а содержимое — стабильный якорь.
type Case struct {
	Question string   `json:"question"`
	Expect   []string `json:"expect"`
}

// norm — нормализация для сравнения: нижний регистр + ё → е.
// В книгах «ё» пишут непоследовательно, и сравнение «Дариэн» ↔ «дариэн»
// не должно зависеть от этого.
func norm(s string) string {
	return strings.ReplaceAll(strings.ToLower(s), "ё", "е")
}

func search(ctx context.Context, st *store.Store, prov llm.Provider, mode, q string, k int) ([]store.Hit, error) {
	switch mode {
	case "fts": // канал без LLM: эмбеддер не нужен
		return st.KeywordSearch(ctx, q, k)
	case "vector":
		vecs, err := prov.Embed(ctx, []string{q})
		if err != nil {
			return nil, err
		}
		return st.VectorSearch(ctx, vecs[0], k)
	case "hybrid":
		vecs, err := prov.Embed(ctx, []string{q})
		if err != nil {
			return nil, err
		}
		return st.HybridSearch(ctx, vecs[0], q, k)
	default:
		return nil, fmt.Errorf("неизвестный режим: %q (доступны fts, vector, hybrid)", mode)
	}
}

func main() {
	casesPath := flag.String("cases", "testdata/bench.json", "JSON с контрольными вопросами")
	mode := flag.String("mode", "hybrid", "режим поиска: hybrid | fts | vector")
	k := flag.Int("k", 8, "глубина оценки (Hit@k)")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		fmt.Println("config:", err)
		os.Exit(1)
	}

	raw, err := os.ReadFile(*casesPath)
	if err != nil {
		fmt.Printf("не удалось прочитать %s: %v\n", *casesPath, err)
		fmt.Println("создайте файл по образцу из testdata/bench.json.example")
		os.Exit(1)
	}
	var cases []Case
	if err := json.Unmarshal(raw, &cases); err != nil {
		fmt.Println("битый JSON:", err)
		os.Exit(1)
	}
	if len(cases) == 0 {
		fmt.Println("в файле нет кейсов")
		os.Exit(1)
	}

	st, err := store.New(cfg.DBPath)
	if err != nil {
		fmt.Println("store:", err)
		os.Exit(1)
	}
	defer st.Close()

	// Для режима fts эмбеддер не нужен — можно проверять канал изолированно.
	var prov llm.Provider
	if *mode != "fts" {
		prov = llm.NewOllama(cfg.LLMURL, cfg.EmbedModel, "", cfg.HTTPTimeout)
		if _, err := prov.Embed(context.Background(), []string{"ping"}); err != nil {
			fmt.Printf("эмбеддер %q недоступен: %v\n", cfg.EmbedModel, err)
			fmt.Println("запустите Ollama или используйте -mode fts")
			os.Exit(1)
		}
	}

	fmt.Printf("режим: %s · k=%d · кейсов: %d\n\n", *mode, *k, len(cases))

	var hitCount int
	var mrrSum float64
	for _, c := range cases {
		start := time.Now()
		hits, err := search(context.Background(), st, prov, *mode, c.Question, *k)
		elapsed := time.Since(start)
		if err != nil {
			fmt.Printf("[ERR ] %-50s ошибка: %v\n", c.Question, err)
			continue
		}

		// Оценка кейса: первый фрагмент топа, содержащий любую из подстрок.
		found, rank, what, frag := false, 0, "", ""
		for i, h := range hits {
			nt := norm(h.Text)
			for _, e := range c.Expect {
				if strings.Contains(nt, norm(e)) {
					found, rank, what = true, i+1, e
					frag = fmt.Sprintf("%s · фрагм. %d", h.DocName, h.Seq+1)
					break
				}
			}
			if found {
				break
			}
		}

		marker, extra := "MISS", ""
		if found {
			marker = "OK  "
			mrrSum += 1.0 / float64(rank)
			hitCount++
			extra = fmt.Sprintf("ранг %d · %s · совпало «%s»", rank, frag, what)
		}
		fmt.Printf("[%s] %-45s %6.2fs  %s\n", marker, c.Question, elapsed.Seconds(), extra)
	}

	fmt.Printf("\nHit@%d: %d/%d (%.0f%%)   MRR: %.3f\n",
		*k, hitCount, len(cases), 100*float64(hitCount)/float64(len(cases)), mrrSum/float64(len(cases)))
}
