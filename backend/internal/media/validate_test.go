package media

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/google/uuid"
)

var (
	jpegBytes = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("jpeg-fixture")...)
	pngBytes  = append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, []byte("png-fixture")...)
	mp4Bytes  = append([]byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, []byte("mp4-fixture")...)
	gpBytes   = append([]byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', '3', 'g', 'p', '4'}, []byte("3gp-fixture")...)
	gifBytes  = []byte("GIF89a-not-allowed")
	limits    = Limits{MaxImageBytes: 64, MaxVideoBytes: 128}
)

func TestSHA256Hex_KnownVectors(t *testing.T) {
	cases := map[string]string{
		"":    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"abc": "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
	}
	for in, want := range cases {
		if got := SHA256Hex([]byte(in)); got != want {
			t.Fatalf("SHA256(%q) = %s, want %s", in, got, want)
		}
	}
	if SHA256Hex(jpegBytes) != SHA256Hex(append([]byte(nil), jpegBytes...)) {
		t.Fatal("same bytes must hash the same")
	}
}

func TestDetectMIME(t *testing.T) {
	for b, want := range map[string]string{
		string(jpegBytes): "image/jpeg", string(pngBytes): "image/png", string(mp4Bytes): "video/mp4",
		string(gpBytes): "video/3gpp", string(gifBytes): "", "": "", "<html>": "",
	} {
		if got := DetectMIME([]byte(b)); got != want {
			t.Fatalf("DetectMIME(%q) = %q, want %q", b[:min(len(b), 8)], got, want)
		}
	}
}

func TestValidate(t *testing.T) {
	v, err := Validate(KindImage, jpegBytes, limits)
	if err != nil || v.MimeType != "image/jpeg" || v.Extension != ".jpg" || v.Size != int64(len(jpegBytes)) || v.SHA256 != SHA256Hex(jpegBytes) {
		t.Fatalf("valid jpeg: %+v %v", v, err)
	}
	if v, err := Validate(KindVideo, gpBytes, limits); err != nil || v.MimeType != "video/3gpp" {
		t.Fatalf("valid 3gp: %+v %v", v, err)
	}
	bad := []struct {
		kind Kind
		data []byte
		want error
	}{
		{KindImage, nil, ErrEmpty},
		{KindImage, append(jpegBytes, make([]byte, 64)...), ErrTooLarge},
		{KindImage, gifBytes, ErrUnsupportedMIME},
		{KindImage, mp4Bytes, ErrKindMismatch}, // declared image, bytes are video
		{KindVideo, pngBytes, ErrKindMismatch},
		{Kind("audio"), jpegBytes, ErrUnsupportedKind},
	}
	for _, c := range bad {
		if _, err := Validate(c.kind, c.data, limits); !errors.Is(err, c.want) {
			t.Fatalf("Validate(%s, %d bytes) = %v, want %v", c.kind, len(c.data), err, c.want)
		}
	}
}

func TestStorageKey_DeterministicVendorScopedAndSafe(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	sha := SHA256Hex(jpegBytes)
	k1, k2 := StorageKey(a, sha, ".jpg"), StorageKey(a, sha, ".jpg")
	if k1 != k2 {
		t.Fatal("same vendor + same bytes must give the same key")
	}
	if StorageKey(b, sha, ".jpg") == k1 {
		t.Fatal("different vendors must not share a key")
	}
	if k1 != "vendors/"+a.String()+"/media/"+sha+".jpg" || strings.Contains(k1, "..") {
		t.Fatalf("unexpected key %q", k1)
	}
}

func TestSupportedKind(t *testing.T) {
	for k, want := range map[string]bool{"image": true, "video": true, "audio": false, "document": false, "sticker": false} {
		if SupportedKind(k) != want {
			t.Fatalf("SupportedKind(%q) != %v", k, want)
		}
	}
}

// Real (tiny, generated) image files, not just magic-byte headers.
func TestValidate_RealEncodedFixtures(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := 0; x < 4; x++ {
		img.Set(x, x, color.RGBA{200, 30, 90, 255})
	}
	var pb, jb bytes.Buffer
	if err := png.Encode(&pb, img); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&jb, img, nil); err != nil {
		t.Fatal(err)
	}
	big := Limits{MaxImageBytes: 1 << 20, MaxVideoBytes: 1 << 20}
	for name, c := range map[string]struct {
		data []byte
		mime string
	}{"png": {pb.Bytes(), "image/png"}, "jpeg": {jb.Bytes(), "image/jpeg"}} {
		v, err := Validate(KindImage, c.data, big)
		if err != nil || v.MimeType != c.mime || v.SHA256 != SHA256Hex(c.data) {
			t.Fatalf("%s fixture: %+v %v", name, v, err)
		}
	}
	// A minimal ISO-BMFF header as recorded by phones (ftyp box, mp42 brand).
	mp4 := append([]byte{0, 0, 0, 0x20, 'f', 't', 'y', 'p', 'm', 'p', '4', '2', 0, 0, 0, 0, 'm', 'p', '4', '2', 'i', 's', 'o', 'm'}, make([]byte, 16)...)
	if v, err := Validate(KindVideo, mp4, big); err != nil || v.MimeType != "video/mp4" || v.Extension != ".mp4" {
		t.Fatalf("mp4 fixture: %+v %v", v, err)
	}
}
