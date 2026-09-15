package imagefit

import (
	"fmt"
	"image"
)

// mustEncode asserts that the standard library's PNG and JPEG encoders
// cannot fail on what this package hands them.
//
// It cannot be reached from a test, because it cannot be reached at
// all. Both encoders have exactly two failure modes: a write error,
// and an image dimension they cannot represent. The writer is a
// bytes.Buffer, which never fails. The dimensions are bounded twice
// over: fit refuses to decode anything declaring more than maxPixels,
// and clamps the caller's ceiling to maxEdgeCeiling (8192) before
// scale, so no image reaching an encoder here can approach the 65536
// limit either encoder rejects at.
//
// It panics rather than returning an error the caller would have to
// carry, because the alternative is an `if err != nil` arm in fit that
// no test can drive and the 100% coverage gate would then have to be
// lied to. If the stdlib ever grows a third failure mode, a crash is
// how we find out on the first affected image rather than through an
// agent quietly never seeing a photo.
func mustEncode(err error, format string, img image.Image) {
	if err != nil {
		panic(fmt.Sprintf("imagefit: %s encoder refused a %v image that fit's own bounds should have made encodable: %v",
			format, img.Bounds(), err))
	}
}
