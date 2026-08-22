package engine

import (
	"database/sql/driver"
	"reflect"
	"time"

	"github.com/oxisto/lightsql/internal/catalog"
	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/types"
)

// FromDriver converts a database/sql argument into an engine value.
//
// The set of types accepted here is exactly the set database/sql guarantees
// after its own conversion step, so an unexpected type is a programming error
// rather than user input.
func FromDriver(v driver.Value) (types.Value, error) {
	switch v := v.(type) {
	case nil:
		return types.Null(), nil
	case bool:
		return types.Bool(v), nil
	case int64:
		return types.Int(v), nil
	case float64:
		return types.Float(v), nil
	case string:
		return types.Text(v), nil
	case []byte:
		return types.Bytea(v), nil
	case time.Time:
		return types.TimeValue(v, types.KindTimestamptz), nil
	default:
		return types.Value{}, pgerr.Newf(pgerr.DatatypeMismatch,
			"unsupported argument type %T", v)
	}
}

// ToDriver converts an engine value into something database/sql can scan.
//
// Every returned type is one of driver.Value's permitted set. Handing back
// anything else, such as a uint64, makes database/sql fall back to reflection
// and gives the caller a confusing error at Scan time rather than here.
func ToDriver(v types.Value) driver.Value {
	switch v.Kind() {
	case types.KindNull:
		return nil
	case types.KindBool:
		return v.AsBool()
	case types.KindInt:
		return v.AsInt()
	case types.KindFloat:
		return v.AsFloat()
	case types.KindText:
		return v.AsString()
	case types.KindBytea, types.KindJSON, types.KindJSONB:
		// json comes back as bytes, as lib/pq and pgx return it, so that
		// json.Unmarshal can consume a scan destination directly. Scanning
		// into a string still works, since database/sql converts bytes to it.
		return v.AsBytes()
	case types.KindNumeric:
		// As text, which is what lib/pq and pgx do, and for the same reason:
		// driver.Value has no exact decimal, so anything else would round on
		// the way out and undo the point of the column. database/sql converts
		// it for a caller scanning into a float64 or a string, so both still
		// work -- but the digits are all there for one that wants them.
		return v.String()
	case types.KindDate, types.KindTime, types.KindTimestamp, types.KindTimestamptz:
		return v.AsTime()
	default:
		// Named rather than defaulted. A kind that lands here by falling
		// through reads its payload as microseconds and returns a plausible
		// timestamp for whatever it actually was, which is how a NUMERIC(5,2)
		// holding 9.5 once came back as 1970-01-01 00:00:00.000095.
		return v.String()
	}
}

// ScanType reports the Go type a column is normally scanned into, for
// driver.RowsColumnTypeScanType.
//
// Nullable columns report the sql.Null* shaped type via interface{} because a
// NULL cannot be scanned into a plain int64; reporting the plain type would
// mislead an ORM into generating a scan that fails on the first NULL.
func ScanType(t catalog.Type, nullable bool) reflect.Type {
	if nullable {
		return reflect.TypeFor[any]()
	}
	switch t.Kind {
	case types.KindBool:
		return reflect.TypeFor[bool]()
	case types.KindInt:
		return reflect.TypeFor[int64]()
	case types.KindFloat:
		return reflect.TypeFor[float64]()
	case types.KindNumeric:
		// A string, matching what ToDriver hands back. Reporting float64 would
		// invite a generated scan that rounds away the exactness the column
		// exists for.
		return reflect.TypeFor[string]()
	case types.KindText:
		return reflect.TypeFor[string]()
	case types.KindBytea, types.KindJSON, types.KindJSONB:
		return reflect.TypeFor[[]byte]()
	case types.KindDate, types.KindTime, types.KindTimestamp, types.KindTimestamptz:
		return reflect.TypeFor[time.Time]()
	default:
		return reflect.TypeFor[any]()
	}
}
