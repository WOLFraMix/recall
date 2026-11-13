package store

import (
	"context"
	"strings"
	"unicode"
)

// Hit — результат поиска: чанк + оценка релевантности + имя документа
// (для отображения источника в интерфейсе).
type Hit struct {
	ChunkID int64
	DocID   int64
	DocName string
	Seq     int
	Text    string
	Score   float64
}

// KeywordSearch — полнотекстовый поиск через FTS5.
// MATCH принимает query-синтаксис FTS: слова, префиксы, фразы в кавычках.
// bm25() — стандартная метрика релевантности полнотекстовых движков:
// чем реже слово встречается в коллекции и чем чаще в конкретном чанке,
// тем выше оценка. ORDER BY bm25(chunks_fts) — ASC, потому что
// меньшее значение = лучше совпадение.
func (s *Store) KeywordSearch(ctx context.Context, query string, k int) ([]Hit, error) {
	words := strings.Fields(query)
	if len(words) == 0 {
		return nil, nil
	}

	terms := make([]string, 0, len(words))
	for _, w := range words {
		// Оставляем только буквы и цифры: пунктуация и кавычки
		// ломают FTS-синтаксис, чистый trimmed-токен безопасен и без кавычек.
		w = strings.ToLower(strings.TrimFunc(w, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		}))
		r := []rune(w)
		if len(r) == 0 {
			continue
		}
		if len(r) > 4 {
			// Псевдо-стемминг: unicode61 не умеет русскую морфологию,
			// «машина» и «машине» для него разные слова. Префиксный
			// оператор * покрывает падежные формы ценой лёгкого шума.
			terms = append(terms, string(r[:len(r)-1])+"*")
		} else {
			terms = append(terms, `"`+strings.ReplaceAll(w, `"`, `""`)+`"`)
		}
	}
	if len(terms) == 0 {
		return nil, nil
	}
	match := strings.Join(terms, " ")

	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.doc_id, d.name, c.seq, c.text, bm25(chunks_fts) AS score
		FROM chunks_fts f
		JOIN chunks c ON c.id = f.chunk_id
		JOIN documents d ON d.id = c.doc_id
		WHERE chunks_fts MATCH ?
		ORDER BY score ASC
		LIMIT ?`, match, k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.ChunkID, &h.DocID, &h.DocName, &h.Seq, &h.Text, &h.Score); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// VectorSearch — семантический поиск: перебираем все чанки, считаем
// косинусную близость с вектором вопроса. O(N) по числу чанков —
// для личного архива (десятки тысяч) это миллисекунды на любом CPU.
// Именно так работали первые версии векторных БД; анн-индексы (HNSW
// и подобные) нужны, когда N = миллионы — отличный пункт «Limitations».
func (s *Store) VectorSearch(ctx context.Context, queryVec []float32, k int) ([]Hit, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.doc_id, d.name, c.seq, c.text, c.embedding
		FROM chunks c
		JOIN documents d ON d.id = c.doc_id
		WHERE d.status = 'ready'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var all []Hit
	for rows.Next() {
		var h Hit
		var blob []byte
		if err := rows.Scan(&h.ChunkID, &h.DocID, &h.DocName, &h.Seq, &h.Text, &blob); err != nil {
			return nil, err
		}
		h.Score = cosine(queryVec, decodeVec(blob))
		all = append(all, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Сортировка по убыванию близости, отрезаем топ-k.
	sortByScoreDesc(all)
	if len(all) > k {
		all = all[:k]
	}
	return all, nil
}

// HybridSearch — слияние двух поисков через Reciprocal Rank Fusion.
// Не смотрим на «сырые» оценки (bm25 и косинус — несравнимые шкалы!),
// а учитываем только ПОЗИЦИИ в каждом списке: score = Σ 1/(k + rank_i).
// Константа k=60 сглаживает разницу между первым и десятым местом.
func (s *Store) HybridSearch(ctx context.Context, queryVec []float32, query string, k int) ([]Hit, error) {
	// Берём k*2 из каждого поиска: fusion отрежет лишнее,
	// а на вход её лучше кормить шире, чем итоговый топ.
	kw, err := s.KeywordSearch(ctx, query, k*2)
	if err != nil {
		return nil, err
	}
	vec, err := s.VectorSearch(ctx, queryVec, k*2)
	if err != nil {
		return nil, err
	}

	const rrfK = 60.0
	scores := map[int64]float64{} // chunk_id → накопленный RRF-балл
	bests := map[int64]Hit{}      // chunk_id → лучший Hit (с текстом)

	// add — замыкание: одна логика начисления очков для обоих списков.
	// rank считается с 1: первое место даёт 1/(60+1) — самый жирный вклад.
	add := func(hits []Hit) {
		for i, h := range hits {
			rank := i + 1
			scores[h.ChunkID] += 1.0 / (rrfK + float64(rank))
			// Из двух списков, где встретился чанк, храним экземпляр
			// с более полным текстом — он пойдёт в цитату.
			if prev, ok := bests[h.ChunkID]; !ok || len(h.Text) > len(prev.Text) {
				bests[h.ChunkID] = h
			}
		}
	}
	add(kw)
	add(vec)

	merged := make([]Hit, 0, len(bests))
	for id, h := range bests {
		h.Score = scores[id]
		merged = append(merged, h)
	}
	sortByScoreDesc(merged)
	if len(merged) > k {
		merged = merged[:k]
	}
	return merged, nil
}
