package types

import (
	"encoding/binary"

	"github.com/oxisto/lightsql/internal/pgerr"
)

// The codec lives in this package rather than in the write-ahead log because
// the layout of a Value is this package's business. Encoding it from outside
// would mean either exporting the three fields, which invites code that builds
// a Value with a payload its kind does not name, or reaching for reflection on
// a per-value path.

// maxEncodedLen bounds the length prefix of a string-shaped value, so a corrupt
// record cannot make the decoder allocate a gigabyte before it fails. It is far
// above any value lightsql is meant to hold and far below what would hurt.
const maxEncodedLen = 1 << 30

// AppendValue appends the encoding of v to dst and returns the extended slice,
// following the append convention of the standard library so that a caller can
// encode a whole row into one buffer.
//
// The scalar kinds are written as a fixed eight bytes rather than as a varint.
// A varint would be shorter for small positive integers and longer for every
// negative one, since the payload word is the raw bit pattern -- the sign bit of
// an int64 and the exponent of a float64 both land in the high bits. One
// unconditional path is worth more here than a size win on half the values.
func AppendValue(dst []byte, v Value) []byte {
	dst = append(dst, byte(v.k))
	switch v.k {
	case KindNull:
		return dst
	case KindText, KindBytea, KindJSON, KindJSONB:
		dst = binary.AppendUvarint(dst, uint64(len(v.s)))
		return append(dst, v.s...)
	default:
		return binary.LittleEndian.AppendUint64(dst, v.n)
	}
}

// DecodeValue reads one value from the front of src, returning it together with
// the remaining bytes.
//
// Every length and kind read from src is checked. The bytes come from a file
// that a crash may have truncated mid-record, so this is an ordinary case
// rather than a corrupted-memory one, and it must fail rather than panic.
func DecodeValue(src []byte) (Value, []byte, error) {
	if len(src) == 0 {
		return Value{}, nil, errTruncated
	}
	k := Kind(src[0])
	src = src[1:]
	if int(k) >= len(kindNames) {
		return Value{}, nil, pgerr.Newf(pgerr.DataCorrupted, "unknown value kind %d", k)
	}

	switch k {
	case KindNull:
		return Null(), src, nil
	case KindText, KindBytea, KindJSON, KindJSONB:
		n, read := binary.Uvarint(src)
		if read <= 0 {
			return Value{}, nil, errTruncated
		}
		if read != uvarintLen(n) {
			// binary.Uvarint accepts a padded encoding, so 0 can arrive as one
			// byte or as ten. Refusing the long forms keeps the encoding
			// canonical, which is what lets the log's checksum stand for the
			// content rather than merely for the bytes.
			return Value{}, nil, pgerr.New(pgerr.DataCorrupted, "value length is not minimally encoded")
		}
		src = src[read:]
		if n > maxEncodedLen {
			return Value{}, nil, pgerr.Newf(pgerr.DataCorrupted, "value length %d is out of range", n)
		}
		if uint64(len(src)) < n {
			return Value{}, nil, errTruncated
		}
		return Value{k: k, s: string(src[:n])}, src[n:], nil
	default:
		if len(src) < 8 {
			return Value{}, nil, errTruncated
		}
		return Value{k: k, n: binary.LittleEndian.Uint64(src)}, src[8:], nil
	}
}

// uvarintLen returns the number of bytes binary.AppendUvarint would use for n.
func uvarintLen(n uint64) int {
	size := 1
	for n >= 0x80 {
		n >>= 7
		size++
	}
	return size
}

// errTruncated reports a value that runs off the end of its buffer. It is one
// shared error rather than one per call site because the caller acts on all of
// them identically: a truncated record is discarded, not repaired.
var errTruncated = pgerr.New(pgerr.DataCorrupted, "value is truncated")
