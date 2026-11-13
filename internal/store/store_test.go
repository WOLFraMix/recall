package store

import (
	"math"
	"testing"
)

func TestCosine(t *testing.T) {
	cases := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical", []float32{1, 2, 3}, []float32{1, 2, 3}, 1},
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0},
		{"opposite", []float32{1, 0}, []float32{-1, 0}, -1},
	}
	for _, c := range cases {
		got := cosine(c.a, c.b)
		if math.Abs(got-c.want) > 1e-6 {
			t.Errorf("%s: cosine = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestVecEncoding(t *testing.T) {
	v := []float32{1.5, -2.25, 0}
	if got := decodeVec(encodeVec(v)); got[0] != 1.5 || got[1] != -2.25 || got[2] != 0 {
		t.Fatalf("round-trip failed: %v", got)
	}
}
