package ocidist

import (
	"crypto/sha256"
	"encoding/hex"
	"io"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// maxLayerBytes caps one module pulled from a registry. An artifact from
// somewhere else is untrusted input, and a layer that claims to be small can
// decompress to anything.
const maxLayerBytes = 256 << 20

func readLayer(l v1.Layer) ([]byte, error) {
	rc, err := l.Uncompressed()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(io.LimitReader(rc, maxLayerBytes))
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
