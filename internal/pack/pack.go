// Package pack turns a chunk into a replica unique to one miner and one
// position, so that one stored copy cannot be claimed by several addresses and
// a missing copy cannot be cheaply rebuilt during mining.
//
//	seed   = Balloon(Tagged("cb/pack", bucket, position, miner), space, rounds)
//	stream = AES-256-CTR(seed) over a full 256 KiB chunk
//	packed = (chunk padded to 256 KiB) XOR stream
//
// The memory-hard Balloon step is the expensive part; the stream can be
// sought, so a verifier unpacks a single 8 KiB sub-chunk.
package pack

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/params"
)

// Seed derives the packing key for one chunk position and miner.
func Seed(p *params.Params, bucket uint32, position uint64, miner cryptox.Address) cryptox.Hash {
	in := cryptox.Tagged("cb/pack", cryptox.U64(uint64(bucket)), cryptox.U64(position), miner[:])
	return cryptox.Balloon(in[:], []byte("cb/pack/salt"), p.BalloonSpace, p.BalloonRounds)
}

func xorAt(seed cryptox.Hash, offset int, data []byte) []byte {
	block, err := aes.NewCipher(seed[:])
	if err != nil {
		panic(err) // 32-byte key cannot fail
	}
	var iv [16]byte
	binary.BigEndian.PutUint64(iv[8:], uint64(offset/aes.BlockSize))
	out := make([]byte, len(data))
	cipher.NewCTR(block, iv[:]).XORKeyStream(out, data)
	return out
}

// Pack returns the 256 KiB packed replica of a chunk.
func Pack(seed cryptox.Hash, chunk []byte) []byte {
	full := make([]byte, params.ChunkSize)
	copy(full, chunk)
	return xorAt(seed, 0, full)
}

// UnpackSub unpacks sub-chunk s from its packed bytes and trims it to length.
func UnpackSub(seed cryptox.Hash, s int, packedSub []byte, length int) []byte {
	raw := xorAt(seed, s*params.SubChunkSize, packedSub)
	if length < len(raw) {
		raw = raw[:length]
	}
	return raw
}

// Unpack recovers a whole chunk of the given length.
func Unpack(seed cryptox.Hash, packed []byte, length int) []byte {
	raw := xorAt(seed, 0, packed)
	return raw[:length]
}
