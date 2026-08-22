// Package wal is lightsql's write-ahead log: the on-disk record of everything a
// committed transaction changed, and the means of replaying it after a restart.
//
// The log is logical rather than physical. A record names a table and carries
// row values or the text of a DDL statement, not a page image, so the on-disk
// format does not have to change every time an in-memory structure does. That
// matters more here than the compactness a physical log would buy: lightsql
// keeps its data in memory anyway, and the log exists to reconstruct that state
// rather than to be read from during a query.
package wal

import (
	"encoding/binary"

	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/types"
)

// RecordKind says what a record describes.
type RecordKind uint8

const (
	// DDL is a schema statement, logged as the text that was executed.
	//
	// Replaying the statement rebuilds the catalog exactly, including the
	// DEFAULT expressions, CHECK constraints and partial-index predicates that
	// the catalog itself stores as syntax. Encoding those structurally would
	// mean a second serialisation of the whole AST, which would then have to be
	// kept in step with the parser for no gain.
	DDL RecordKind = iota
	// Insert adds a row with a given identity.
	Insert
	// Delete removes the row with that identity. An update is logged as a
	// delete of the old row followed by an insert of the new one, which is what
	// the storage layer does in memory as well.
	Delete
	// Missing records the value that rows written before an ADD COLUMN take for
	// the new column. It follows the DDL record for the ALTER statement.
	//
	// It has to be logged rather than recomputed, because ADD COLUMN evaluates
	// the column's DEFAULT once at the moment it runs. Replaying
	// `ADD COLUMN c timestamp DEFAULT now()` would evaluate now() again and
	// give every old row a value from recovery time instead of from the day the
	// column was added.
	Missing
)

var recordKindNames = [...]string{DDL: "ddl", Insert: "insert", Delete: "delete", Missing: "missing"}

func (k RecordKind) String() string {
	if int(k) < len(recordKindNames) {
		return recordKindNames[k]
	}
	return "RecordKind(" + string(rune('0'+k)) + ")"
}

// Record is one logged change.
//
// The fields a record uses depend on its kind, which is why they are one struct
// rather than an interface: the set is small, closed, and decoded in a loop
// that would otherwise allocate per record.
type Record struct {
	Kind RecordKind
	// SQL is the statement text, for DDL.
	SQL string
	// Table is the schema-qualified table name, for the row kinds and Missing.
	Table string
	// Row identifies the row within its table, for Insert and Delete.
	Row uint64
	// Column is the ordinal the Missing value belongs to.
	Column uint32
	// Vals is the row, for Insert, or the single missing value, for Missing.
	Vals []types.Value
}

// DDLRecord returns a record for a schema statement.
func DDLRecord(sql string) Record { return Record{Kind: DDL, SQL: sql} }

// InsertRecord returns a record for a row written into table.
func InsertRecord(table string, row uint64, vals []types.Value) Record {
	return Record{Kind: Insert, Table: table, Row: row, Vals: vals}
}

// DeleteRecord returns a record for a row removed from table.
func DeleteRecord(table string, row uint64) Record {
	return Record{Kind: Delete, Table: table, Row: row}
}

// MissingRecord returns a record fixing the value short rows take for a column
// added by ALTER TABLE.
func MissingRecord(table string, column int, v types.Value) Record {
	return Record{Kind: Missing, Table: table, Column: uint32(column), Vals: []types.Value{v}}
}

// maxLen bounds every count and length read back from the log, so a corrupt
// header cannot make the decoder reserve a gigabyte before it discovers the
// record is nonsense.
const maxLen = 1 << 30

func appendRecord(dst []byte, r *Record) []byte {
	dst = append(dst, byte(r.Kind))
	switch r.Kind {
	case DDL:
		return appendString(dst, r.SQL)
	case Insert:
		dst = appendString(dst, r.Table)
		dst = binary.AppendUvarint(dst, r.Row)
		dst = binary.AppendUvarint(dst, uint64(len(r.Vals)))
		for _, v := range r.Vals {
			dst = types.AppendValue(dst, v)
		}
		return dst
	case Delete:
		dst = appendString(dst, r.Table)
		return binary.AppendUvarint(dst, r.Row)
	default:
		dst = appendString(dst, r.Table)
		dst = binary.AppendUvarint(dst, uint64(r.Column))
		return types.AppendValue(dst, r.Vals[0])
	}
}

func decodeRecord(src []byte) (Record, []byte, error) {
	if len(src) == 0 {
		return Record{}, nil, errTruncated
	}
	r := Record{Kind: RecordKind(src[0])}
	src = src[1:]
	if int(r.Kind) >= len(recordKindNames) {
		return Record{}, nil, pgerr.Newf(pgerr.DataCorrupted, "unknown record kind %d", r.Kind)
	}

	var err error
	switch r.Kind {
	case DDL:
		r.SQL, src, err = decodeString(src)
		return r, src, err
	case Insert:
		if r.Table, src, err = decodeString(src); err != nil {
			return Record{}, nil, err
		}
		if r.Row, src, err = decodeUvarint(src); err != nil {
			return Record{}, nil, err
		}
		n, src, err := decodeUvarint(src)
		if err != nil {
			return Record{}, nil, err
		}
		if n > maxLen {
			return Record{}, nil, pgerr.Newf(pgerr.DataCorrupted, "row has %d columns", n)
		}
		r.Vals = make([]types.Value, n)
		for i := range r.Vals {
			if r.Vals[i], src, err = types.DecodeValue(src); err != nil {
				return Record{}, nil, err
			}
		}
		return r, src, nil
	case Delete:
		if r.Table, src, err = decodeString(src); err != nil {
			return Record{}, nil, err
		}
		r.Row, src, err = decodeUvarint(src)
		return r, src, err
	default:
		if r.Table, src, err = decodeString(src); err != nil {
			return Record{}, nil, err
		}
		col, src, err := decodeUvarint(src)
		if err != nil {
			return Record{}, nil, err
		}
		if col > maxLen {
			return Record{}, nil, pgerr.Newf(pgerr.DataCorrupted, "column ordinal %d is out of range", col)
		}
		r.Column = uint32(col)
		v, src, err := types.DecodeValue(src)
		if err != nil {
			return Record{}, nil, err
		}
		r.Vals = []types.Value{v}
		return r, src, nil
	}
}

func appendString(dst []byte, s string) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

func decodeString(src []byte) (s string, rest []byte, err error) {
	n, src, err := decodeUvarint(src)
	if err != nil {
		return "", nil, err
	}
	if n > maxLen {
		return "", nil, pgerr.Newf(pgerr.DataCorrupted, "string length %d is out of range", n)
	}
	if uint64(len(src)) < n {
		return "", nil, errTruncated
	}
	return string(src[:n]), src[n:], nil
}

// decodeUvarint reads a length or count, refusing the padded encodings
// binary.Uvarint would otherwise accept. Keeping the format canonical is what
// lets the frame checksum stand for the content rather than merely for the
// bytes; types.DecodeValue refuses them for the same reason.
func decodeUvarint(src []byte) (n uint64, rest []byte, err error) {
	n, read := binary.Uvarint(src)
	if read <= 0 {
		return 0, nil, errTruncated
	}
	if read != uvarintLen(n) {
		return 0, nil, pgerr.New(pgerr.DataCorrupted, "length is not minimally encoded")
	}
	return n, src[read:], nil
}

func uvarintLen(n uint64) int {
	size := 1
	for n >= 0x80 {
		n >>= 7
		size++
	}
	return size
}

var errTruncated = pgerr.New(pgerr.DataCorrupted, "record is truncated")
