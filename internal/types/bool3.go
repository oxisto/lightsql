package types

// Bool3 is SQL's three-valued boolean: TRUE, FALSE, or UNKNOWN.
//
// Comparisons return Bool3 rather than bool on purpose. In SQL, NULL = NULL is
// UNKNOWN, not TRUE, and a WHERE clause keeps a row only when its predicate is
// TRUE — UNKNOWN and FALSE both drop it, but they differ under NOT. Making the
// third value part of the type means a caller cannot accidentally collapse it,
// which is how "NULL = NULL is true" bugs get written.
type Bool3 uint8

const (
	// False and True are the ordinary two values.
	False Bool3 = iota
	True
	// Unknown is the result of any comparison involving NULL.
	Unknown
)

// Bool3Of converts a Go bool.
func Bool3Of(b bool) Bool3 {
	if b {
		return True
	}
	return False
}

// IsTrue reports whether b is True. This is the test a WHERE, ON or HAVING
// clause applies: UNKNOWN does not pass a filter.
func (b Bool3) IsTrue() bool { return b == True }

// IsUnknown reports whether b is Unknown.
func (b Bool3) IsUnknown() bool { return b == Unknown }

// Not implements SQL's NOT: NOT UNKNOWN is UNKNOWN.
func (b Bool3) Not() Bool3 {
	switch b {
	case True:
		return False
	case False:
		return True
	default:
		return Unknown
	}
}

// And implements SQL's AND. FALSE dominates, so FALSE AND UNKNOWN is FALSE.
func (b Bool3) And(o Bool3) Bool3 {
	if b == False || o == False {
		return False
	}
	if b == Unknown || o == Unknown {
		return Unknown
	}
	return True
}

// Or implements SQL's OR. TRUE dominates, so TRUE OR UNKNOWN is TRUE.
func (b Bool3) Or(o Bool3) Bool3 {
	if b == True || o == True {
		return True
	}
	if b == Unknown || o == Unknown {
		return Unknown
	}
	return False
}

// Value converts b to a SQL value: Unknown becomes NULL.
func (b Bool3) Value() Value {
	if b == Unknown {
		return Null()
	}
	return Bool(b == True)
}

func (b Bool3) String() string {
	switch b {
	case True:
		return "true"
	case False:
		return "false"
	default:
		return "unknown"
	}
}
