package media

import (
	"bytes"
	"image"
	"image/png"
	"testing"
)

func TestReencode(t *testing.T) {
	var b bytes.Buffer
	png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 8, 8)))
	// Bytes appended after the image end are a classic place to hide a payload.
	in := append(b.Bytes(), []byte("<script>payload</script>")...)
	out, class, err := Reencode(in)
	if err != nil || class == 0 {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("payload")) {
		t.Fatal("hidden payload survived re-encoding")
	}
	if _, _, err := Reencode([]byte("MZ\x90\x00 this is an executable")); err == nil {
		t.Fatal("executable accepted")
	}
}
