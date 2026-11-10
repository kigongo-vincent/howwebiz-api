package catalog

import (
	"sync"
	"testing"
)

// Every provider crawler ingests at once, so Fold runs on many goroutines.
// A shared, stateful transformer here once crashed the whole process with
// "slice bounds out of range" (run with -race to catch regressions early).
func TestFoldConcurrent(t *testing.T) {
	inputs := map[string]string{
		"Éddy Kénzo":                  "eddy kenzo",
		"Sitya Loss (Official Audio)": "sitya loss official audio",
		"Bébé Cool & Azawi":           "bebe cool and azawi",
		"Ñandú – Ça Va":               "nandu ca va",
		"plain ascii":                 "plain ascii",
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 2000 {
				for in, want := range inputs {
					if got := Fold(in); got != want {
						t.Errorf("Fold(%q) = %q, want %q", in, got, want)
						return
					}
				}
			}
		})
	}
	wg.Wait()
}
