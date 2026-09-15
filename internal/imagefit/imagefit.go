// Package imagefit shrinks an image to a maximum long-edge pixel
// dimension so it can be inlined into a model prompt.
//
// # Why it exists
//
// A byte cap is not a pixel cap, and the model providers enforce the
// latter. Anthropic refuses a request outright when it carries several
// images and ANY of them exceeds 2000 pixels on a side:
//
//	400 … image.source.base64.data: At least one of the image
//	dimensions exceed max allowed size for many-image requests:
//	2000 pixels
//
// A modern phone photo is 5712x4284 and compresses to well under a
// megabyte, so it sails past every byte budget and then hard-kills the
// turn. Worse, it kills every LATER turn in the same conversation,
// because the oversized image stays in the session's history and is
// re-sent with each prompt. The only fix that holds is to never put an
// oversized image in front of the agent in the first place.
//
// The ceiling is a pixel budget as much as a compatibility one:
// Anthropic's own guidance is that no useful detail is gained above
// ~1568 pixels on the long edge, and everything above it is paid for
// in tokens and latency for nothing. DefaultMaxEdge is therefore well
// under the 2000 hard limit rather than at it.
//
// # What it does NOT do
//
// It never touches a file on disk. The relay stores the ORIGINAL bytes
// in the conversation's inbox and downscales only the copy that goes
// into the prompt, because an agent doing detail work — reading a book
// page out of a photo, say — opens the file itself and must get the
// full resolution the human sent.
//
// It is deliberately free of any chat-protocol import: the same
// problem exists in every relay that inlines an image, so this is a
// promotion candidate for acp-kit (see BACKLOG.md), and the import
// graph is what keeps that option open.
package imagefit

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"

	xdraw "golang.org/x/image/draw"

	// Decode-only formats. A GIF or a WebP is re-encoded as JPEG.
	// WebP is not optional: Android screenshots and several phone
	// exports arrive as WebP, and a format we cannot decode is a
	// format we cannot shrink — the oversized bytes would go straight
	// into the prompt and the request would be rejected exactly as
	// before.
	_ "golang.org/x/image/webp"
	_ "image/gif"
)

const (
	// DefaultMaxEdge is the long-edge ceiling applied when an operator
	// has not chosen one: Anthropic's recommended maximum useful
	// dimension, comfortably below the 2000px many-image hard limit.
	DefaultMaxEdge = 1568
	// jpegQuality is what a downscaled photo is re-encoded at. 85 is
	// the usual "no visible loss at a glance" point, and the image has
	// already lost far more information to the resampling than it does
	// to the quantiser.
	jpegQuality = 85
	// maxPixels bounds what will be DECODED at all, independent of the
	// byte cap upstream. Compression ratios are unbounded — a small
	// file can declare a gigapixel canvas — and decoding allocates
	// 4 bytes per pixel regardless of the file's size. 50 megapixels
	// is twice the largest consumer camera and costs ~200 MB if
	// someone actually sends one; the caller may hand over several
	// images from one message, so this is a number that has to stay
	// survivable when multiplied.
	maxPixels = 50 << 20
	// maxEdgeCeiling clamps the operator's ceiling. It is not taste:
	// JPEG and PNG both refuse a dimension of 65536 or more, and an
	// image that only needed its orientation baked in is otherwise
	// handed to the encoder at whatever size it arrived at. Clamping
	// the request is what makes both encoders total, which is why
	// their error arms live in imagefit_must.go.
	maxEdgeCeiling = 8192
)

// MIME types this package produces. A downscaled image is always one
// of these two: PNG survives as PNG so a screenshot's text stays
// crisp, and everything else becomes JPEG.
const (
	MIMEPNG  = "image/png"
	MIMEJPEG = "image/jpeg"
)

// Fit returns a copy of data whose long edge is at most maxEdge,
// together with the MIME type of the returned bytes.
//
// It returns the input unchanged — same bytes, same mimeType — when
// there is nothing to do or nothing it can do: maxEdge <= 0 (the
// operator disabled downscaling), the image already fits, or the
// bytes are not a format it can decode.
//
// It does NOT compare byte sizes. A re-encode can come out LARGER
// than a heavily optimised original — stdlib png.Encode against an
// oxipng'd screenshot, say — and handing back the original because it
// was smaller would hand back the oversized dimensions this package
// exists to remove. Bytes are the caller's budget; pixels are the
// thing that makes the request illegal.
//
// It never returns an error. Every failure is a reason to inline the
// original, which is what the relay did before this existed; a turn
// must not fail because a resize did. The caller is told what happened
// only through the returned mimeType and byte count.
func Fit(data []byte, mimeType string, maxEdge int) ([]byte, string) {
	out, outMIME, err := fit(data, mimeType, maxEdge)
	if err != nil {
		return data, mimeType
	}
	return out, outMIME
}

// fit is Fit with the errors still visible, so tests can assert on the
// reason rather than on the fallback.
func fit(data []byte, mimeType string, maxEdge int) ([]byte, string, error) {
	if maxEdge <= 0 {
		return nil, "", fmt.Errorf("downscaling disabled")
	}
	maxEdge = min(maxEdge, maxEdgeCeiling)
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("decode config: %w", err)
	}
	// Multiplied as int64 on purpose: `int` is 32 bits on the armv6
	// build, and a header declaring 100000x100000 would otherwise
	// overflow to a negative product, pass this check, and then be
	// allocated.
	if int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return nil, "", fmt.Errorf("image declares %dx%d, over the %d pixel decode limit", cfg.Width, cfg.Height, maxPixels)
	}
	// EXIF orientation is read BEFORE the fits-already test, because a
	// sideways photo's long edge swaps when the tag is applied, and
	// because re-encoding drops the tag: an image we touch at all must
	// be baked upright or it reaches the model rotated.
	orient := jpegOrientation(data)
	if cfg.Width <= maxEdge && cfg.Height <= maxEdge && orient == 1 {
		return nil, "", fmt.Errorf("already fits")
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("decode: %w", err)
	}
	// Scale FIRST, then rotate. The long-edge ceiling is invariant
	// under transposition, so the result is identical either way —
	// but applyOrientation is a per-pixel loop, and running it on the
	// 1568px copy instead of the 24-megapixel original is two orders
	// of magnitude less work.
	dst := applyOrientation(scale(src, maxEdge), orient)

	out, outMIME := encode(dst, format)
	return out, outMIME, nil
}

// encode re-encodes at the format the source came in as, collapsed to
// the two the model providers all accept. PNG stays PNG: it is what a
// screenshot arrives as, it is lossless, and JPEG artefacts around
// text are exactly what would make the downscale visible. Everything
// else — JPEG, GIF — becomes JPEG.
func encode(img image.Image, format string) ([]byte, string) {
	var buf bytes.Buffer
	if format == "png" {
		mustEncode(png.Encode(&buf, img), "png", img)
		return buf.Bytes(), MIMEPNG
	}
	// A GIF or a transparent source flattens onto white rather than
	// onto JPEG's default black, which is what a human would expect of
	// a screenshot or a logo.
	mustEncode(jpeg.Encode(&buf, flatten(img), &jpeg.Options{Quality: jpegQuality}), "jpeg", img)
	return buf.Bytes(), MIMEJPEG
}

// scale resamples img so its long edge is at most maxEdge, preserving
// aspect ratio. An image already within the ceiling is returned as it
// is — this is reached when only the orientation needed baking in.
//
// CatmullRom is the choice because this is a large downscale of
// photographic content: it is sharper than bilinear at the 3-4x
// reduction a phone photo needs, and the cost is irrelevant next to
// the round trip it is saving.
func scale(img image.Image, maxEdge int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxEdge && h <= maxEdge {
		return img
	}
	if w >= h {
		h = max(1, h*maxEdge/w)
		w = maxEdge
	} else {
		w = max(1, w*maxEdge/h)
		h = maxEdge
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Over, nil)
	return dst
}

// flatten composites img over white, so transparency does not become
// black on the way into JPEG.
func flatten(img image.Image) image.Image {
	b := img.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(out, out.Bounds(), img, b.Min, draw.Over)
	return out
}

// applyOrientation bakes an EXIF orientation into the pixels. 1 —
// and the 0 that means "no tag" — leave the image untouched. The
// range is 1..8: jpegOrientation is the only source, and it
// normalises anything outside it away.
func applyOrientation(img image.Image, orient int) image.Image {
	if orient <= 1 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	// 5..8 are the transposed cases: the output's axes are swapped.
	outW, outH := w, h
	if orient >= 5 {
		outW, outH = h, w
	}
	out := image.NewRGBA(image.Rect(0, 0, outW, outH))
	for y := range h {
		for x := range w {
			var dx, dy int
			switch orient {
			case 2: // mirror horizontal
				dx, dy = w-1-x, y
			case 3: // rotate 180
				dx, dy = w-1-x, h-1-y
			case 4: // mirror vertical
				dx, dy = x, h-1-y
			case 5: // mirror horizontal + rotate 270 CW
				dx, dy = y, x
			case 6: // rotate 90 CW
				dx, dy = h-1-y, x
			case 7: // mirror horizontal + rotate 90 CW
				dx, dy = h-1-y, w-1-x
			default: // 8: rotate 270 CW
				dx, dy = y, w-1-x
			}
			out.Set(dx, dy, img.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return out
}

// jpegOrientation reads the EXIF Orientation tag out of a JPEG's APP1
// segment, returning 1 ("upright") for anything it cannot read.
//
// This is a deliberately tiny reader rather than a dependency. It
// walks the JPEG marker chain to the first APP1/Exif segment, then
// reads exactly one IFD0 entry — tag 0x0112. Nothing here follows an
// offset into a sub-IFD or trusts a length it has not bounds-checked
// against the slice it was handed.
func jpegOrientation(data []byte) int {
	exif, ok := app1(data)
	if !ok {
		return 1
	}
	// TIFF header: byte order, 0x002A, offset of IFD0.
	if len(exif) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(exif[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	off := int(bo.Uint32(exif[4:8]))
	// Each IFD is a uint16 count followed by count 12-byte entries.
	// Written as len-2 rather than off+2: `int` is 32 bits on the
	// armv6 build, where a crafted offset near MaxInt32 would wrap
	// negative, pass an off+2 check, and panic on the slice.
	if off < 8 || off > len(exif)-2 {
		return 1
	}
	n := int(bo.Uint16(exif[off : off+2]))
	off += 2
	for i := 0; i < n; i++ {
		if off > len(exif)-12 {
			return 1
		}
		e := exif[off : off+12]
		off += 12
		// Tag 0x0112, type 3 (SHORT): the value is small enough to sit
		// in the entry's own value field rather than at an offset.
		if bo.Uint16(e[:2]) != 0x0112 || bo.Uint16(e[2:4]) != 3 {
			continue
		}
		// Normalised here, once: buggy writers do emit 0 and values
		// above 8, and an unnormalised one would fail the "already
		// fits" test in fit and force a pointless re-encode of an
		// image that was fine.
		if v := int(bo.Uint16(e[8:10])); v >= 1 && v <= 8 {
			return v
		}
		return 1
	}
	return 1
}

// app1 returns the bytes after the "Exif\0\0" identifier of a JPEG's
// first APP1 segment.
func app1(data []byte) ([]byte, bool) {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, false // not a JPEG
	}
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xFF {
			return nil, false // out of step with the marker chain
		}
		marker := data[i+1]
		// SOS (0xDA) starts entropy-coded data: no more metadata
		// segments follow, and the chain stops being walkable.
		if marker == 0xDA {
			return nil, false
		}
		size := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if size < 2 || i+2+size > len(data) {
			return nil, false
		}
		seg := data[i+4 : i+2+size]
		if marker == 0xE1 && len(seg) >= 6 && string(seg[:6]) == "Exif\x00\x00" {
			return seg[6:], true
		}
		i += 2 + size
	}
	return nil, false
}
