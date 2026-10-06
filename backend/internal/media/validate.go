package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Limits are the per-kind maximum sizes in bytes (configurable; defaults
// follow WhatsApp's own inbound limits: images 5 MB, video 16 MB).
type Limits struct {
	MaxImageBytes int64
	MaxVideoBytes int64
}

// MaxBytes returns the limit for a kind (0 for unsupported kinds).
func (l Limits) MaxBytes(k Kind) int64 {
	switch k {
	case KindImage:
		return l.MaxImageBytes
	case KindVideo:
		return l.MaxVideoBytes
	}
	return 0
}

// SupportedKind reports whether 4A.1 ingests this MediaRef kind. Audio,
// document and sticker refs stay as MediaRef only (not downloaded).
func SupportedKind(kind string) bool {
	return kind == string(KindImage) || kind == string(KindVideo)
}

// allowed content types per kind, detected from the bytes. The set is
// code, not configuration: a type can only be allowed if DetectMIME can
// recognise it.
var allowed = map[Kind]map[string]string{ // mime -> file extension
	KindImage: {"image/jpeg": ".jpg", "image/png": ".png"},
	KindVideo: {"video/mp4": ".mp4", "video/3gpp": ".3gp"},
}

// DetectMIME identifies the content type from magic bytes. It recognises
// only what 4A.1 accepts (JPEG, PNG, ISO-BMFF MP4/3GPP) and returns "" for
// anything else. Headers, filenames and provider metadata are not trusted.
func DetectMIME(b []byte) string {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 8 && bytes.Equal(b[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return "image/png"
	case len(b) >= 12 && string(b[4:8]) == "ftyp":
		brand := string(b[8:12])
		if brand[:3] == "3gp" || brand[:3] == "3g2" {
			return "video/3gpp"
		}
		return "video/mp4"
	}
	return ""
}

// Validated is a downloaded payload that passed ingestion validation.
type Validated struct {
	MimeType  string
	Extension string
	SHA256    string // lowercase hex of the exact bytes
	Size      int64
}

// Validate checks size, content type and kind consistency against the
// actual bytes, and computes the SHA-256. All failures are permanent: the
// same bytes would fail again.
func Validate(kind Kind, data []byte, limits Limits) (*Validated, error) {
	max := limits.MaxBytes(kind)
	if max <= 0 {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedKind, kind)
	}
	if len(data) == 0 {
		return nil, ErrEmpty
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%w: %d > %d bytes", ErrTooLarge, len(data), max)
	}
	mime := DetectMIME(data)
	if mime == "" {
		return nil, ErrUnsupportedMIME
	}
	ext, ok := allowed[kind][mime]
	if !ok {
		return nil, fmt.Errorf("%w: %s content for %s", ErrKindMismatch, mime, kind)
	}
	return &Validated{MimeType: mime, Extension: ext, SHA256: SHA256Hex(data), Size: int64(len(data))}, nil
}

// SHA256Hex is the lowercase hex SHA-256 of b.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// StorageKey is the deterministic, Majo-generated storage location of a
// vendor's asset: the same vendor + same bytes always map to the same key,
// so retries (and duplicate sends) overwrite one object instead of piling
// up orphans. It contains only a UUID and a hash; no vendor-controlled text
// (filename, caption) ever reaches a storage path.
func StorageKey(vendorID fmt.Stringer, sha256Hex, ext string) string {
	return fmt.Sprintf("vendors/%s/media/%s%s", vendorID.String(), sha256Hex, ext)
}
