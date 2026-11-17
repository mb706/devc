package ctxrepo

import "testing"

// TestDecodeStrict: a helper's report followed by anything but white space
// is refused, a stray ']' or '}' included (finding G4-24).
func TestDecodeStrict(t *testing.T) {
	var v struct{ A int }
	if err := decodeStrict([]byte("{\"A\":1}\n"), &v); err != nil || v.A != 1 {
		t.Errorf("valid report: %v", err)
	}
	for _, bad := range []string{`{"A":1}]`, `{"A":1}}`, `{"A":1} {}`, `{"A":1} x`} {
		if err := decodeStrict([]byte(bad), &v); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
