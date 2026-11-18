// Package media checks and re-encodes uploads before they reach the chain.
// Re-encoding rebuilds the file from decoded pixels, which drops metadata
// (location, camera) and any payload hidden in the original bytes. This is
// node and client policy, not consensus: the chain only sees data roots.
package media

import (
	"bytes"
	"errors"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"

	"github.com/namsaeg/chainboard/internal/types"
)

// MaxPixels bounds decoded image size so a tiny file cannot expand into
// gigabytes of memory.
const MaxPixels = 40_000_000

var ErrUnsupported = errors.New("media: only PNG, JPEG and GIF images are accepted in 0.1.0")

// Sniff returns the media class of data, or an error if it is not accepted.
func Sniff(b []byte) (uint8, string, error) {
	ct := http.DetectContentType(b)
	switch ct {
	case "image/png", "image/jpeg":
		return types.MediaImage, ct, nil
	case "image/gif":
		return types.MediaGIF, ct, nil
	}
	return 0, ct, ErrUnsupported
}

// Reencode validates and rebuilds an image. It returns the new bytes and the
// media class.
func Reencode(in []byte) ([]byte, uint8, error) {
	class, ct, err := Sniff(in)
	if err != nil {
		return nil, 0, err
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(in))
	if err != nil {
		return nil, 0, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > MaxPixels {
		return nil, 0, errors.New("media: image dimensions out of range")
	}
	var out bytes.Buffer
	switch ct {
	case "image/gif":
		g, err := gif.DecodeAll(bytes.NewReader(in))
		if err != nil {
			return nil, 0, err
		}
		g.Config.ColorModel = nil // rebuilt from frames
		if err := gif.EncodeAll(&out, g); err != nil {
			return nil, 0, err
		}
	case "image/png":
		img, err := png.Decode(bytes.NewReader(in))
		if err != nil {
			return nil, 0, err
		}
		enc := png.Encoder{CompressionLevel: png.BestCompression}
		if err := enc.Encode(&out, img); err != nil {
			return nil, 0, err
		}
	case "image/jpeg":
		img, err := jpeg.Decode(bytes.NewReader(in))
		if err != nil {
			return nil, 0, err
		}
		if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 85}); err != nil {
			return nil, 0, err
		}
	}
	return out.Bytes(), class, nil
}
