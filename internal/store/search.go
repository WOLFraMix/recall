package store

import (
	"context"
	"regexp"
	"sort"
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

// phraseRe — извлекает фразы в кавычках: пользователь имеет право
// искать точное сочетание слов, а не россыпь терминов.
var phraseRe = regexp.MustCompile(`"([^"]+)"`)

// russianStop — служебные слова русского языка. В FTS они не должны
// становиться термами: bm25 понижает их вес в СВОЕЙ шкале, но RRF видит
// только позиции — а частотные слова дают им позиции, забивая канал
// шумом. Урок бенчмарка: «что делает это приложение?» через OR выдало
// 16 чанков книги по слову «что», и они вытеснили README с векторного
// ранга 1. Стоп-слова режут шум до того, как он попал в fusion.
var russianStop = map[string]struct{}{
	"а": {}, "без": {}, "больше": {}, "будет": {}, "будто": {}, "бы": {},
	"был": {}, "была": {}, "были": {}, "было": {}, "быть": {}, "в": {},
	"вам": {}, "вас": {}, "вдруг": {}, "ведь": {}, "во": {}, "вот": {},
	"все": {}, "всего": {}, "всех": {}, "вы": {}, "где": {}, "да": {},
	"даже": {}, "для": {}, "до": {}, "его": {}, "ее": {}, "ей": {},
	"её": {}, "если": {}, "есть": {}, "еще": {}, "ещё": {}, "же": {},
	"за": {}, "здесь": {}, "и": {}, "из": {}, "или": {}, "им": {},
	"их": {}, "к": {}, "как": {}, "когда": {}, "кто": {}, "куда": {},
	"ли": {}, "лишь": {}, "либо": {}, "мне": {}, "может": {}, "мы": {},
	"на": {}, "над": {}, "не": {}, "него": {}, "нее": {}, "неё": {},
	"нет": {}, "ни": {}, "нибудь": {}, "но": {}, "о": {}, "об": {},
	"однако": {}, "он": {}, "она": {}, "они": {}, "опять": {}, "от": {},
	"по": {}, "под": {}, "пока": {}, "потом": {}, "потому": {}, "при": {},
	"про": {}, "просто": {}, "пусть": {}, "раз": {}, "с": {}, "сам": {},
	"свой": {}, "себе": {}, "себя": {}, "со": {}, "совсем": {}, "так": {},
	"также": {}, "такой": {}, "там": {}, "тем": {}, "теперь": {}, "то": {},
	"тогда": {}, "того": {}, "тоже": {}, "той": {}, "только": {}, "том": {},
	"тот": {}, "тут": {}, "ты": {}, "у": {}, "уже": {}, "хоть": {},
	"чего": {}, "чем": {}, "через": {}, "что": {}, "чтобы": {}, "эти": {},
	"это": {}, "этот": {}, "я": {},
}

// IsStopword — проверка, является ли слово служебным (для инструментов,
// которые должны повторять логику FTS-канала: ftscheck, подсветка).
func IsStopword(w string) bool {
	_, ok := russianStop[strings.ToLower(w)]
	return ok
}

// KeywordSearch — полнотекстовый канал гибридного поиска.
// Два фильтра шума, каждый — урок из бенчмарка:
//  1. стоп-слова: служебные слова не становятся термами;
//  2. адаптивный DF-фильтр: основа, встречающаяся более чем в 5%
//     чанков корпуса, не несёт различительного смысла (в романе это
//     «делае*», в техдокументации — то же слово сильный термин!
//     Поэтому фильтр считаем по КАЖДОМУ корпусу, а не фиксируем
//     списком). Без него частотные основы заполняли выдачу и
//     вытесняли сильные векторные совпадения из fusion.
func (s *Store) KeywordSearch(ctx context.Context, query string, k int) ([]Hit, error) {
	phrases := phraseRe.FindAllString(query, -1)
	rest := phraseRe.ReplaceAllString(query, " ")

	// Кандидаты: фразы (явный выбор пользователя — не фильтруем)
	// и одиночные слова (стоп-слова отсекаем, длинные — в префикс).
	type cand struct {
		term  string
		isPhr bool
	}
	var cands []cand
	for _, p := range phrases {
		p = strings.Join(strings.Fields(p), " ")
		if p == "" {
			continue
		}
		p = strings.ReplaceAll(p, `"`, `""`)
		cands = append(cands, cand{`"` + p + `"`, true})
	}
	for _, w := range strings.Fields(rest) {
		w = strings.ToLower(strings.TrimFunc(w, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		}))
		if w == "" {
			continue
		}
		if _, stop := russianStop[w]; stop {
			continue
		}
		r := []rune(w)
		if len(r) == 0 {
			continue
		}
		if len(r) > 4 {
			cands = append(cands, cand{string(r[:len(r)-1]) + "*", false})
		} else {
			cands = append(cands, cand{`"` + strings.ReplaceAll(w, `"`, `""`) + `"`, false})
		}
	}
	if len(cands) == 0 {
		return nil, nil
	}

	// DF-фильтр: порог = max(5, 5% от числа чанков). Считать count на
	// базу из тысячи чанков — микросекунды, кэшировать незачем.
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks`).Scan(&total); err != nil {
		return nil, err
	}
	dfLimit := total * 5 / 100
	if dfLimit < 5 {
		dfLimit = 5
	}

	var kept []string
	type droppedTerm struct {
		term string
		df   int
	}
	var dropped []droppedTerm
	for _, c := range cands {
		if c.isPhr {
			kept = append(kept, c.term)
			continue
		}
		var df int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH ?`, c.term,
		).Scan(&df); err != nil {
			continue // битый терм не роняет канал — просто пропускаем
		}
		if df <= dfLimit {
			kept = append(kept, c.term)
		} else {
			dropped = append(dropped, droppedTerm{c.term, df})
		}
	}

	// Все термы частотные (вопрос целиком из частотных слов) — берём
	// два самых редких: пусть канал молчит осмысленно, а не нулём.
	if len(kept) == 0 && len(dropped) > 0 {
		sort.Slice(dropped, func(i, j int) bool { return dropped[i].df < dropped[j].df })
		for i, d := range dropped {
			if i == 2 {
				break
			}
			kept = append(kept, d.term)
		}
	}
	if len(kept) == 0 {
		return nil, nil
	}
	match := strings.Join(kept, " OR ")

	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.doc_id, d.name, c.seq, c.text, bm25(chunks_fts) AS score
		FROM chunks_fts f
		JOIN chunks c ON c.id = f.rowid
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

	// RRF с весами каналов. Голый RRF (все веса = 1) ставит чанк,
	// попавший лишь на 5-е место FTS, выше чанка с 1-м местом FTS,
	// если первый вдобавок есть в векторном списке: 1/61 + 1/62 > 1/61.
	// Это убивало точные совпадения: README («на вашей машине») вылетал
	// из топа, потому что «машинально» из книги матчится префиксом И
	// семантически близко к «машине». Вес 2 за FTS — «попал в оба» уже
	// не перекрывает «первое место в точном поиске».
	const (
		rrfK      = 60.0
		weightFTS = 2.0
		weightVec = 1.0
	)
	scores := map[int64]float64{}
	bests := map[int64]Hit{}

	add := func(hits []Hit, w float64) {
		for i, h := range hits {
			scores[h.ChunkID] += w / (rrfK + float64(i+1))
			if prev, ok := bests[h.ChunkID]; !ok || len(h.Text) > len(prev.Text) {
				bests[h.ChunkID] = h
			}
		}
	}
	add(kw, weightFTS)
	add(vec, weightVec)

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
