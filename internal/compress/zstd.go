// Package compress wraps the compression applied to patch payloads.
//
// Patches are text and compress extremely well; zstd beats gzip on both ratio
// and speed for this shape of data.
//
// Decompression is always bounded. A compressed patch arrives from a client,
// and a few kilobytes of crafted zstd can expand to gigabytes: refusing to
// allocate past a declared ceiling is the difference between rejecting a bad
// request and losing the server.
package compress

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"

	"github.com/NitScm/nit/pkg/protocol"
)

// ErrTooLarge is returned when a payload expands past the allowed size.
var ErrTooLarge = errors.New("compress: decompressed payload too large")

// Compress encodes data with the given encoding.
func Compress(data []byte, encoding protocol.Encoding) ([]byte, error) {
	switch encoding {
	case protocol.EncodingNone:
		return data, nil

	case protocol.EncodingZstd, "":
		var buf bytes.Buffer

		w, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedDefault))
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			w.Close()
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}

		return buf.Bytes(), nil

	default:
		return nil, fmt.Errorf("compress: unsupported encoding %q", encoding)
	}
}

// Decompress decodes data, refusing to produce more than maxSize bytes.
//
// maxSize of zero means unlimited and must only be used on data the server
// produced itself.
func Decompress(data []byte, encoding protocol.Encoding, maxSize int64) ([]byte, error) {
	switch encoding {
	case protocol.EncodingNone:
		if maxSize > 0 && int64(len(data)) > maxSize {
			return nil, ErrTooLarge
		}
		return data, nil

	case protocol.EncodingZstd, "":
		r, err := zstd.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer r.Close()

		if maxSize <= 0 {
			return io.ReadAll(r)
		}

		// Read one byte past the ceiling: reaching it proves the payload is
		// over the limit, without ever holding much more than the limit.
		out, err := io.ReadAll(io.LimitReader(r, maxSize+1))
		if err != nil {
			return nil, err
		}
		if int64(len(out)) > maxSize {
			return nil, ErrTooLarge
		}

		return out, nil

	default:
		return nil, fmt.Errorf("compress: unsupported encoding %q", encoding)
	}
}
