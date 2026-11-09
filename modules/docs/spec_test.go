package docs

import "testing"

func TestSpecParses(t *testing.T) {
	spec, err := Spec()
	if err != nil {
		t.Fatal(err)
	}
	if spec["openapi"] != "3.0.3" || len(spec["paths"].(map[string]any)) == 0 {
		t.Fatalf("unexpected spec header")
	}
}
