// cmd/embedcheck — измеряет, насколько осмысленно эмбеддер понимает
// русские фразы. Берём слово и три фразы: две связанные по смыслу,
// одну — отвлекающую. Хороший мультиязычный эмбеддер даёт связанным
// фразам заметно более высокий косинус, чем отвлекающей. Плохой —
// даёт всем примерно одинаковые малые числа: семантика = шум.
package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/WOLFraMix/recall/internal/llm"
)

func cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
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

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: go run ./cmd/embedcheck <embed-model>")
		os.Exit(2)
	}
	p := llm.NewOllama("http://localhost:11434", os.Args[1], "", time.Minute)
	phrases := []string{
		"машина", // запрос
		"автомобиль едет по дороге",                               // близко по смыслу
		"всё работает на вашей машине, ничего не уходит в облако", // та же словоформа
		"леди Дарзин сидела у камина и пила чай",                  // отвлекающая
	}
	vecs, err := p.Embed(context.Background(), phrases)
	if err != nil {
		fmt.Println("ошибка:", err)
		os.Exit(1)
	}
	for i := 1; i < len(phrases); i++ {
		fmt.Printf("cos(машина, %d) = %.3f   // %s\n", i, cosine(vecs[0], vecs[i]), phrases[i])
	}
}
