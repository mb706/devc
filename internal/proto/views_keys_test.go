package proto

import "testing"

// TestSessionKeys: sessions are named by project and ID; a bare ID is
// still accepted (finding C1-01).
func TestSessionKeys(t *testing.T) {
	const sid = "s0000000001aaaaaaaaaa"
	if k := SessionKey("demo/hello", sid); k != "demo/hello/"+sid {
		t.Errorf("key %q", k)
	}
	ref, id, err := ParseSessionKey("demo/hello/" + sid)
	if err != nil || ref.String() != "demo/hello" || id != sid {
		t.Errorf("parse key: %v %q %v", ref, id, err)
	}
	ref, id, err = ParseSessionKey(sid)
	if err != nil || ref.Realm != "" || id != sid {
		t.Errorf("parse bare: %v %q %v", ref, id, err)
	}
	for _, bad := range []string{"", "x", "demo/" + sid, "../hello/" + sid, "demo/hello/x", "demo/hello/" + sid + "/", "/demo/hello/" + sid} {
		if _, _, err := ParseSessionKey(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := (Cursor{"demo/hello/" + sid: 3, sid: 1}).Validate(); err != nil {
		t.Errorf("cursor: %v", err)
	}
	if err := (Cursor{"demo/" + sid: 3}).Validate(); err == nil {
		t.Error("a cursor with half a project was accepted")
	}
}
