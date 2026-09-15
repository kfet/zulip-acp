package imagefit

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// gradient paints a w x h image whose colour varies in both axes, so a
// resample cannot accidentally look correct and an orientation swap is
// visible in the corner pixels.
func gradient(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// Written straight into Pix: at phone-photo sizes a per-pixel Set
	// makes the test itself the slowest thing in the suite.
	for y := range h {
		row := img.Pix[y*img.Stride : y*img.Stride+w*4]
		for x := range w {
			p := row[x*4 : x*4+4]
			p[0], p[1], p[2], p[3] = uint8(x), uint8(y), 0x40, 0xFF
		}
	}
	return img
}

func encJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

func encPNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func dims(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return cfg.Width, cfg.Height
}

// TestFitDownscalesAPhonePhoto is the case the package exists for: a
// 24-megapixel photo passes every byte budget and is still rejected by
// the provider for its pixel dimensions.
func TestFitDownscalesAPhonePhoto(t *testing.T) {
	src := encJPEG(t, gradient(2400, 1800))
	out, mimeType := Fit(src, "image/jpeg", DefaultMaxEdge)
	if mimeType != MIMEJPEG {
		t.Fatalf("mime = %q", mimeType)
	}
	w, h := dims(t, out)
	if w != DefaultMaxEdge {
		t.Fatalf("width = %d, want %d", w, DefaultMaxEdge)
	}
	// Aspect ratio preserved, within one pixel of rounding.
	if want := 1800 * DefaultMaxEdge / 2400; h != want {
		t.Fatalf("height = %d, want %d", h, want)
	}
	if len(out) >= len(src) {
		t.Fatalf("downscaled to %d bytes from %d", len(out), len(src))
	}
}

// TestFitPortraitUsesTheLongEdge: the ceiling is the LONG edge, not
// the width.
func TestFitPortraitUsesTheLongEdge(t *testing.T) {
	out, _ := Fit(encJPEG(t, gradient(500, 2000)), "image/jpeg", 500)
	w, h := dims(t, out)
	if h != 500 || w != 125 {
		t.Fatalf("got %dx%d, want 125x500", w, h)
	}
}

// TestFitKeepsPNGAsPNG: a screenshot must not pick up JPEG artefacts
// around its text.
func TestFitKeepsPNGAsPNG(t *testing.T) {
	out, mimeType := Fit(encPNG(t, gradient(900, 600)), "image/png", 300)
	if mimeType != MIMEPNG {
		t.Fatalf("mime = %q", mimeType)
	}
	if w, _ := dims(t, out); w != 300 {
		t.Fatalf("width = %d", w)
	}
}

// TestFitReencodesGIFAsJPEG: a decodable format we do not re-encode
// natively still reaches the model, as JPEG.
func TestFitReencodesGIFAsJPEG(t *testing.T) {
	var buf bytes.Buffer
	if err := gif.Encode(&buf, gradient(400, 200), nil); err != nil {
		t.Fatalf("encode gif: %v", err)
	}
	out, mimeType := Fit(buf.Bytes(), "image/gif", 200)
	if mimeType != MIMEJPEG {
		t.Fatalf("mime = %q", mimeType)
	}
	if w, _ := dims(t, out); w != 200 {
		t.Fatalf("width = %d", w)
	}
}

// TestFlattensTransparencyOntoWhite: JPEG has no alpha, and black is
// the wrong answer for a logo or a screenshot.
func TestFlattensTransparencyOntoWhite(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 4)) // fully transparent
	out := flatten(src)
	if got := out.At(2, 2); got != (color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}) {
		t.Fatalf("transparent pixel came out %v, want white", got)
	}
}

// TestFitPassesThrough covers every reason to hand back the input
// untouched. Each of them is a case where inlining the original is
// exactly what the relay did before this package existed.
func TestFitPassesThrough(t *testing.T) {
	small := encPNG(t, gradient(10, 10))
	tests := []struct {
		name    string
		data    []byte
		mime    string
		maxEdge int
		reason  string
	}{
		{"disabled", small, "image/png", 0, "downscaling disabled"},
		{"already fits", encJPEG(t, gradient(100, 100)), "image/jpeg", 1568, "already fits"},
		{"not an image", []byte("%PDF-1.7 and then some"), "application/pdf", 1568, "decode config"},
		{"header only", forgedPNGSize(t, 3000, 3000), "image/png", 1568, "decode:"},
		{"truncated past decoding", small[:20], "image/png", 1568, "decode config"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := fit(tc.data, tc.mime, tc.maxEdge); err == nil {
				t.Fatal("fit succeeded, want a stated reason")
			} else if got := err.Error(); !bytes.Contains([]byte(got), []byte(tc.reason)) {
				t.Fatalf("reason = %q, want it to mention %q", got, tc.reason)
			}
			out, mimeType := Fit(tc.data, tc.mime, tc.maxEdge)
			if !bytes.Equal(out, tc.data) || mimeType != tc.mime {
				t.Fatalf("Fit did not hand back the input unchanged (mime %q)", mimeType)
			}
		})
	}
}

// TestFitRefusesADeclaredGigapixel: a decode allocates 4 bytes per
// pixel no matter how small the file claiming those pixels is.
func TestFitRefusesADeclaredGigapixel(t *testing.T) {
	src := encPNG(t, gradient(64, 64))
	// Rewrite the IHDR dimensions in place and fix its CRC, so the
	// header declares a gigapixel canvas the file does not contain.
	binary.BigEndian.PutUint32(src[16:20], 40000)
	binary.BigEndian.PutUint32(src[20:24], 40000)
	fixPNGCRC(src)
	if _, _, err := fit(src, "image/png", 1568); err == nil {
		t.Fatal("decoded a declared gigapixel")
	} else if !bytes.Contains([]byte(err.Error()), []byte("pixel decode limit")) {
		t.Fatalf("err = %v", err)
	}
}

// TestFitDegenerateDimensions is deliberately absent: image.DecodeConfig
// refuses a non-positive dimension for every format registered here, so
// fit never sees one.

// TestScaleFloorsAtOnePixel: an extreme aspect ratio must not resample
// to a zero-width image, which is not encodable.
func TestScaleFloorsAtOnePixel(t *testing.T) {
	out := scale(gradient(4000, 1), 100)
	if b := out.Bounds(); b.Dx() != 100 || b.Dy() != 1 {
		t.Fatalf("got %v, want 100x1", b)
	}
	out = scale(gradient(1, 4000), 100)
	if b := out.Bounds(); b.Dx() != 1 || b.Dy() != 100 {
		t.Fatalf("got %v, want 1x100", b)
	}
}

// --- EXIF orientation ----------------------------------------------------

// withEXIF wraps JPEG bytes in an APP1 Exif segment declaring one
// IFD0 entry: Orientation = orient. bo picks the byte order, since
// both occur in the wild.
func withEXIF(t *testing.T, jpegData []byte, orient uint16, little bool, tag, typ uint16) []byte {
	t.Helper()
	bo := binary.ByteOrder(binary.BigEndian)
	order := "MM"
	if little {
		bo, order = binary.LittleEndian, "II"
	}
	tiff := make([]byte, 8+2+12)
	copy(tiff, order)
	bo.PutUint16(tiff[2:4], 42)
	bo.PutUint32(tiff[4:8], 8)
	bo.PutUint16(tiff[8:10], 1) // one entry
	e := tiff[10:]
	bo.PutUint16(e[0:2], tag)
	bo.PutUint16(e[2:4], typ)
	bo.PutUint32(e[4:8], 1)
	bo.PutUint16(e[8:10], orient)

	payload := append([]byte("Exif\x00\x00"), tiff...)
	seg := make([]byte, 0, len(payload)+4)
	seg = append(seg, 0xFF, 0xE1)
	seg = binary.BigEndian.AppendUint16(seg, uint16(len(payload)+2))
	seg = append(seg, payload...)

	out := append([]byte{}, jpegData[:2]...) // SOI
	out = append(out, seg...)
	return append(out, jpegData[2:]...)
}

// TestOrientationIsBakedIn is the reason orientation is handled here
// at all: re-encoding DROPS the EXIF tag, so an image we touch must
// come out already upright or the model sees it rotated.
func TestOrientationIsBakedIn(t *testing.T) {
	// A wide image tagged "rotate 90 CW" is really a tall one.
	src := withEXIF(t, encJPEG(t, gradient(1000, 500)), 6, false, 0x0112, 3)
	out, _ := Fit(src, "image/jpeg", 500)
	w, h := dims(t, out)
	if w != 250 || h != 500 {
		t.Fatalf("got %dx%d, want 250x500 — orientation was not applied", w, h)
	}
}

// TestOrientationAloneTriggersAReencode: an image already within the
// ceiling still has to be rewritten when it carries an orientation,
// because the tag does not survive the block.
func TestOrientationAloneTriggersAReencode(t *testing.T) {
	src := withEXIF(t, encJPEG(t, gradient(400, 200)), 6, true, 0x0112, 3)
	out, _ := Fit(src, "image/jpeg", 1568)
	w, h := dims(t, out)
	if w != 200 || h != 400 {
		t.Fatalf("got %dx%d, want 200x400", w, h)
	}
}

// TestApplyOrientationAllCases walks every transform, checking the
// output dimensions and that a known corner pixel lands where the tag
// says it should.
func TestApplyOrientationAllCases(t *testing.T) {
	src := gradient(4, 2)
	tests := []struct {
		orient     int
		wantW      int
		wantH      int
		wantX      int // where src(0,0) ends up
		wantY      int
		unchangedP bool
	}{
		{orient: 0, wantW: 4, wantH: 2, unchangedP: true},
		{orient: 1, wantW: 4, wantH: 2, unchangedP: true},
		{orient: 2, wantW: 4, wantH: 2, wantX: 3, wantY: 0},
		{orient: 3, wantW: 4, wantH: 2, wantX: 3, wantY: 1},
		{orient: 4, wantW: 4, wantH: 2, wantX: 0, wantY: 1},
		{orient: 5, wantW: 2, wantH: 4, wantX: 0, wantY: 0},
		{orient: 6, wantW: 2, wantH: 4, wantX: 1, wantY: 0},
		{orient: 7, wantW: 2, wantH: 4, wantX: 1, wantY: 3},
		{orient: 8, wantW: 2, wantH: 4, wantX: 0, wantY: 3},
	}
	for _, tc := range tests {
		out := applyOrientation(src, tc.orient)
		b := out.Bounds()
		if b.Dx() != tc.wantW || b.Dy() != tc.wantH {
			t.Fatalf("orient %d: got %v, want %dx%d", tc.orient, b, tc.wantW, tc.wantH)
		}
		if tc.unchangedP {
			if out != image.Image(src) {
				t.Fatalf("orient %d: image was rewritten", tc.orient)
			}
			continue
		}
		if got, want := out.At(tc.wantX, tc.wantY), src.At(0, 0); got != want {
			t.Fatalf("orient %d: src(0,0) is not at (%d,%d): got %v want %v", tc.orient, tc.wantX, tc.wantY, got, want)
		}
	}
}

// TestJPEGOrientationRejects covers every way the EXIF reader declines
// to answer. All of them mean "upright", because a metadata parser
// must never be able to fail a turn.
func TestJPEGOrientationRejects(t *testing.T) {
	plain := encJPEG(t, gradient(8, 8))
	exif := withEXIF(t, plain, 6, false, 0x0112, 3)

	// Locate the APP1 payload so the cases below can corrupt it.
	tiffAt := bytes.Index(exif, []byte("Exif\x00\x00")) + 6

	corrupt := func(mut func([]byte)) []byte {
		c := append([]byte{}, exif...)
		mut(c)
		return c
	}

	tests := []struct {
		name string
		data []byte
	}{
		{"not a jpeg", []byte("\x89PNG\r\n\x1a\n")},
		{"too short", []byte{0xFF}},
		{"no exif segment", plain},
		{"out of step with the marker chain", []byte{0xFF, 0xD8, 0x00, 0x00, 0x00, 0x00}},
		{"segment length under two", corrupt(func(c []byte) { binary.BigEndian.PutUint16(c[4:6], 1) })},
		{"segment length past the end", corrupt(func(c []byte) { binary.BigEndian.PutUint16(c[4:6], 0xFFFF) })},
		{"unknown byte order", corrupt(func(c []byte) { copy(c[tiffAt:], "XX") })},
		{"ifd offset before the header", corrupt(func(c []byte) { binary.BigEndian.PutUint32(c[tiffAt+4:], 2) })},
		{"ifd offset past the end", corrupt(func(c []byte) { binary.BigEndian.PutUint32(c[tiffAt+4:], 0xFFFF) })},
		{"entries run off the end", corrupt(func(c []byte) {
			binary.BigEndian.PutUint16(c[tiffAt+8:], 500)
			binary.BigEndian.PutUint16(c[tiffAt+10:], 0x0110) // not Orientation
		})},
		{"exif payload too short for a tiff header", func() []byte {
			seg := []byte{0xFF, 0xE1, 0x00, 0x0A, 'E', 'x', 'i', 'f', 0, 0, 0, 0}
			return append(append(append([]byte{}, plain[:2]...), seg...), plain[2:]...)
		}()},
		{"marker chain runs out without SOS", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x04, 0x00, 0x00}},
		{"another tag", withEXIF(t, plain, 6, false, 0x0110, 3)},
		{"wrong type", withEXIF(t, plain, 6, false, 0x0112, 4)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := jpegOrientation(tc.data); got != 1 {
				t.Fatalf("orientation = %d, want 1", got)
			}
		})
	}
}

// TestFitDownscalesAWebP: a format we cannot decode is a format we
// cannot shrink, and Android screenshots arrive as WebP. The fixture
// is a 2400x1800 WebP, over the 2000px limit that rejects the whole
// request.
func TestFitDownscalesAWebP(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "oversized.webp"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	out, mimeType := Fit(src, "image/webp", DefaultMaxEdge)
	if mimeType != MIMEJPEG {
		t.Fatalf("mime = %q, want the re-encoded JPEG", mimeType)
	}
	if w, h := dims(t, out); w != DefaultMaxEdge || h != 1176 {
		t.Fatalf("got %dx%d, want %dx1176", w, h, DefaultMaxEdge)
	}
}

// TestJPEGOrientationNormalises: writers do emit 0 and values above 8,
// and an unnormalised one would force a re-encode of an image that was
// already fine.
func TestJPEGOrientationNormalises(t *testing.T) {
	plain := encJPEG(t, gradient(8, 8))
	for _, v := range []uint16{0, 9, 65535} {
		if got := jpegOrientation(withEXIF(t, plain, v, false, 0x0112, 3)); got != 1 {
			t.Fatalf("orientation %d normalised to %d, want 1", v, got)
		}
	}
}

// TestApp1StopsAtSOS: once entropy-coded data starts there is no
// marker chain left to walk, and walking into it would be reading
// pixel data as segment lengths.
func TestApp1StopsAtSOS(t *testing.T) {
	data := []byte{0xFF, 0xD8, 0xFF, 0xDA, 0x00, 0x02, 0xFF, 0xE1}
	if _, ok := app1(data); ok {
		t.Fatal("walked past SOS")
	}
}

// TestApp1SkipsANonEXIFAPP1: JFIF and XMP both live in APP1-adjacent
// segments, and a short one must be skipped, not misread.
func TestApp1SkipsANonEXIFAPP1(t *testing.T) {
	// An APP1 whose payload is too short to hold the "Exif\0\0" id,
	// followed by a real one.
	plain := encJPEG(t, gradient(8, 8))
	real := withEXIF(t, plain, 3, false, 0x0112, 3)
	stub := []byte{0xFF, 0xE1, 0x00, 0x04, 0x00, 0x00}
	data := append(append(append([]byte{}, real[:2]...), stub...), real[2:]...)
	if got := jpegOrientation(data); got != 3 {
		t.Fatalf("orientation = %d, want 3 — the real segment was not reached", got)
	}
}

// fixPNGCRC recomputes the IHDR chunk's CRC after its dimensions have
// been rewritten, so image.DecodeConfig reads the forged header rather
// than refusing the file outright.
func fixPNGCRC(src []byte) {
	binary.BigEndian.PutUint32(src[29:33], crc32.ChecksumIEEE(src[12:29]))
}

// forgedPNGSize is a tiny PNG whose IHDR claims w x h. DecodeConfig
// believes the header; Decode then fails on the pixel data that is not
// there. It is how the two decode steps are driven apart without
// encoding a large image in a test.
func forgedPNGSize(t *testing.T, w, h int) []byte {
	t.Helper()
	src := encPNG(t, gradient(10, 10))
	binary.BigEndian.PutUint32(src[16:20], uint32(w))
	binary.BigEndian.PutUint32(src[20:24], uint32(h))
	fixPNGCRC(src)
	return src
}

// TestFitClampsAnAbsurdCeiling: maxEdgeCeiling is what makes both
// encoders total (see imagefit_must.go), so an operator asking for a
// ceiling above it must still get an image bounded by it.
func TestFitClampsAnAbsurdCeiling(t *testing.T) {
	// Tagged sideways so it is re-encoded despite already "fitting".
	src := withEXIF(t, encJPEG(t, gradient(20000, 10)), 6, false, 0x0112, 3)
	out, _ := Fit(src, "image/jpeg", 1<<20)
	w, h := dims(t, out)
	if h != maxEdgeCeiling || w != 4 {
		t.Fatalf("got %dx%d, want 4x%d", w, h, maxEdgeCeiling)
	}
}
