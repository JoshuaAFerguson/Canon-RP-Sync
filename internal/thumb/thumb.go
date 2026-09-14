// Package thumb makes small JPEG previews of imported photos for the web UI
// and the companion apps.
//
// Only JPEG is decoded. RAW is deliberately not: decoding CR3 would mean
// pulling in a heavy dependency for a preview the camera already embeds. For a
// RAW file the caller should fall back to the JPEG shot alongside it, or to the
// camera's own thumbnail endpoint while the card is still reachable.
package thumb

import (
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
)

// ErrUnsupported means the source is not a format this package can decode.
var ErrUnsupported = errors.New("thumb: unsupported source format")

// DefaultSize is the longest edge of a generated thumbnail, in pixels.
const DefaultSize = 480

// quality is the JPEG quality of generated thumbnails.
const quality = 82

// Supported reports whether a file extension can be thumbnailed directly.
func Supported(ext string) bool {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "jpg", "jpeg":
		return true
	}
	return false
}

// Generate writes a thumbnail of src to dst, scaled so its longest edge is at
// most size pixels. An image already smaller than size is re-encoded as-is.
func Generate(src, dst string, size int) error {
	if size <= 0 {
		size = DefaultSize
	}
	if !Supported(filepath.Ext(src)) {
		return fmt.Errorf("%w: %s", ErrUnsupported, filepath.Ext(src))
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	img, err := jpeg.Decode(in)
	if err != nil {
		return fmt.Errorf("decode %s: %w", filepath.Base(src), err)
	}
	small := Resize(img, size)

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := jpeg.Encode(out, small, &jpeg.Options{Quality: quality}); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// Resize scales img down so its longest edge is at most size, averaging source
// pixels so the result does not alias the way nearest-neighbour would.
func Resize(img image.Image, size int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return img
	}
	if w <= size && h <= size {
		return img
	}

	dw, dh := w, h
	if w >= h {
		dw = size
		dh = max(1, h*size/w)
	} else {
		dh = size
		dw = max(1, w*size/h)
	}

	// Work in NRGBA so the averaging below sees plain 8-bit channels.
	srcRGBA, ok := img.(*image.NRGBA)
	if !ok {
		conv := image.NewNRGBA(image.Rect(0, 0, w, h))
		draw.Draw(conv, conv.Bounds(), img, b.Min, draw.Src)
		srcRGBA = conv
	}

	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		sy0, sy1 := y*h/dh, (y+1)*h/dh
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for x := 0; x < dw; x++ {
			sx0, sx1 := x*w/dw, (x+1)*w/dw
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}

			var r, g, bl, a, n int
			for sy := sy0; sy < sy1; sy++ {
				row := srcRGBA.PixOffset(sx0, sy)
				for sx := sx0; sx < sx1; sx++ {
					i := row + (sx-sx0)*4
					r += int(srcRGBA.Pix[i])
					g += int(srcRGBA.Pix[i+1])
					bl += int(srcRGBA.Pix[i+2])
					a += int(srcRGBA.Pix[i+3])
					n++
				}
			}
			if n == 0 {
				continue
			}
			o := dst.PixOffset(x, y)
			dst.Pix[o] = uint8(r / n)
			dst.Pix[o+1] = uint8(g / n)
			dst.Pix[o+2] = uint8(bl / n)
			dst.Pix[o+3] = uint8(a / n)
		}
	}
	return dst
}

// Cache returns the path to a cached thumbnail for id, generating it from src
// if it is missing or older than the source.
func Cache(cacheDir, id, src string, size int) (string, error) {
	dst := filepath.Join(cacheDir, id+".jpg")

	srcInfo, err := os.Stat(src)
	if err != nil {
		return "", err
	}
	if dstInfo, err := os.Stat(dst); err == nil && dstInfo.ModTime().After(srcInfo.ModTime()) {
		return dst, nil
	}
	if err := Generate(src, dst, size); err != nil {
		return "", err
	}
	return dst, nil
}
