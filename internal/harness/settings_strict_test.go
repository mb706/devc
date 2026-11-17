package harness

import "testing"

// TestParseSettingsStrict: a JSON settings file with anything but white
// space after its object is refused, a stray ']' or '}' included (finding
// G4-24).
func TestParseSettingsStrict(t *testing.T) {
	if v, err := parseSettings("settings.json", "json", []byte("{\"a\":1}\n")); err != nil || v["a"] == nil {
		t.Errorf("valid settings: %v %v", v, err)
	}
	for _, bad := range []string{`{"a":1}]`, `{"a":1}}`, `{"a":1} {}`, `{"a":1} x`} {
		if _, err := parseSettings("settings.json", "json", []byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
