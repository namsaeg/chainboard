// Package codec is the canonical binary encoding used for everything that is
// hashed or signed. Decoding is strict: non-minimal varints, oversize lengths
// and trailing bytes are rejected, so every value has exactly one encoding.
package codec

import (
	"encoding/binary"
	"errors"
)

var (
	ErrShort    = errors.New("codec: unexpected end of data")
	ErrNonCanon = errors.New("codec: non-canonical varint")
	ErrTooLarge = errors.New("codec: length too large")
	ErrTrailing = errors.New("codec: trailing bytes")
)

// Writer appends encoded values.
type Writer struct{ B []byte }

func (w *Writer) U8(v uint8)     { w.B = append(w.B, v) }
func (w *Writer) U16(v uint16)   { w.B = binary.BigEndian.AppendUint16(w.B, v) }
func (w *Writer) U32(v uint32)   { w.B = binary.BigEndian.AppendUint32(w.B, v) }
func (w *Writer) U64(v uint64)   { w.B = binary.BigEndian.AppendUint64(w.B, v) }
func (w *Writer) Var(v uint64)   { w.B = binary.AppendUvarint(w.B, v) }
func (w *Writer) Fixed(b []byte) { w.B = append(w.B, b...) }
func (w *Writer) Bytes(b []byte) {
	w.Var(uint64(len(b)))
	w.B = append(w.B, b...)
}
func (w *Writer) Bool(v bool) {
	if v {
		w.U8(1)
	} else {
		w.U8(0)
	}
}

// Reader decodes values; the first error sticks and later reads return zero.
type Reader struct {
	B   []byte
	Err error
}

func NewReader(b []byte) *Reader { return &Reader{B: b} }

func (r *Reader) fail(e error) {
	if r.Err == nil {
		r.Err = e
	}
}

func (r *Reader) take(n int) []byte {
	if r.Err != nil {
		return nil
	}
	if n < 0 || len(r.B) < n {
		r.fail(ErrShort)
		return nil
	}
	out := r.B[:n]
	r.B = r.B[n:]
	return out
}

func (r *Reader) U8() uint8 {
	b := r.take(1)
	if b == nil {
		return 0
	}
	return b[0]
}
func (r *Reader) U16() uint16 {
	b := r.take(2)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint16(b)
}
func (r *Reader) U32() uint32 {
	b := r.take(4)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}
func (r *Reader) U64() uint64 {
	b := r.take(8)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}

func (r *Reader) Var() uint64 {
	if r.Err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.B)
	if n <= 0 {
		r.fail(ErrShort)
		return 0
	}
	if n != len(binary.AppendUvarint(nil, v)) {
		r.fail(ErrNonCanon)
		return 0
	}
	r.B = r.B[n:]
	return v
}

func (r *Reader) Bool() bool {
	v := r.U8()
	if v > 1 {
		r.fail(ErrNonCanon)
	}
	return v == 1
}

// Fixed copies exactly len(dst) bytes.
func (r *Reader) Fixed(dst []byte) {
	b := r.take(len(dst))
	if b != nil {
		copy(dst, b)
	}
}

// Bytes reads a length-prefixed byte string of at most max bytes.
func (r *Reader) Bytes(max int) []byte {
	n := r.Var()
	if r.Err != nil {
		return nil
	}
	if n > uint64(max) {
		r.fail(ErrTooLarge)
		return nil
	}
	b := r.take(int(n))
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}

// Count reads a list length of at most max.
func (r *Reader) Count(max int) int {
	n := r.Var()
	if r.Err == nil && n > uint64(max) {
		r.fail(ErrTooLarge)
		return 0
	}
	return int(n)
}

// Done returns the sticky error, or ErrTrailing if bytes are left.
// AtEnd reports whether all input was read.
func (r *Reader) AtEnd() bool { return r.Err == nil && len(r.B) == 0 }

func (r *Reader) Done() error {
	if r.Err != nil {
		return r.Err
	}
	if len(r.B) != 0 {
		return ErrTrailing
	}
	return nil
}
