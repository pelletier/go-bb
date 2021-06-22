package example

import (
	"testing"
)

func BenchmarkMe(b *testing.B) {
	data := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 0}
	b.SetBytes(int64(len(data)) * 8)
	for i := 0; i < b.N; i++ {
		total := 0
		for _, x := range data {
			total += x
		}
	}
}

func TestSomething(t *testing.T) {
	t.Skip()
}
