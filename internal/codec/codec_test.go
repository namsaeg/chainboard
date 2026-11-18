package codec

import "testing"

func TestStrict(t *testing.T) {
	var w Writer
	w.Var(300)
	w.Bytes([]byte("hi"))
	r := NewReader(w.B)
	if r.Var() != 300 || string(r.Bytes(10)) != "hi" || r.Done() != nil {
		t.Fatal("roundtrip")
	}
	if r := NewReader([]byte{0x80, 0x00}); r.Var() != 0 || r.Err == nil {
		t.Fatal("non-canonical varint accepted")
	}
	if r := NewReader(append(w.B, 1)); r.Var() == 300 && r.Bytes(10) != nil && r.Done() == nil {
		t.Fatal("trailing accepted")
	}
}
