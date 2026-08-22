package compress

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/NitScm/nit/pkg/protocol"
)

func TestRoundTrip(t *testing.T) {
	payload := []byte(strings.Repeat("diff --git a/a b/a\n+line\n", 500))

	for _, encoding := range []protocol.Encoding{protocol.EncodingZstd, protocol.EncodingNone} {
		t.Run(string(encoding), func(t *testing.T) {
			compressed, err := Compress(payload, encoding)
			if err != nil {
				t.Fatalf("Compress: %v", err)
			}

			got, err := Decompress(compressed, encoding, 1<<20)
			if err != nil {
				t.Fatalf("Decompress: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Error("round trip did not reproduce the payload")
			}
		})
	}
}

func TestZstdCompressesPatches(t *testing.T) {
	payload := []byte(strings.Repeat("diff --git a/a b/a\n+line\n", 500))

	compressed, err := Compress(payload, protocol.EncodingZstd)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}

	if len(compressed) >= len(payload)/4 {
		t.Errorf("compressed %d bytes to %d; patches should compress far better than that",
			len(payload), len(compressed))
	}
}

// The property that matters: a few kilobytes of crafted zstd must not be able
// to make the server allocate gigabytes.
func TestDecompressRefusesBombs(t *testing.T) {
	bomb, err := Compress(bytes.Repeat([]byte{0}, 50<<20), protocol.EncodingZstd)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}

	if len(bomb) > 1<<20 {
		t.Fatalf("the test payload compressed to %d bytes, too large to be a useful bomb", len(bomb))
	}

	if _, err := Decompress(bomb, protocol.EncodingZstd, 1<<20); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}

func TestDecompressAtLimitSucceeds(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 1024)

	compressed, err := Compress(payload, protocol.EncodingZstd)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}

	if _, err := Decompress(compressed, protocol.EncodingZstd, 1024); err != nil {
		t.Errorf("a payload exactly at the limit must be accepted: %v", err)
	}
}

func TestUncompressedIsAlsoBounded(t *testing.T) {
	if _, err := Decompress(bytes.Repeat([]byte("x"), 100), protocol.EncodingNone, 10); !errors.Is(err, ErrTooLarge) {
		t.Errorf("got %v, want ErrTooLarge", err)
	}
}

func TestUnsupportedEncoding(t *testing.T) {
	if _, err := Compress(nil, "brotli"); err == nil {
		t.Error("Compress accepted an unsupported encoding")
	}
	if _, err := Decompress(nil, "brotli", 0); err == nil {
		t.Error("Decompress accepted an unsupported encoding")
	}
}
