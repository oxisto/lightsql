package main

import (
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// format is how a result set is written out.
type format uint8

const (
	// formatTable is the aligned, human-readable form, laid out the way psql
	// lays it out.
	formatTable format = iota
	// formatCSV writes RFC 4180, for piping into something else.
	formatCSV
	// formatJSON writes one object per row, for the same reason.
	formatJSON
)

func parseFormat(name string) (format, error) {
	switch strings.ToLower(name) {
	case "table", "":
		return formatTable, nil
	case "csv":
		return formatCSV, nil
	case "json":
		return formatJSON, nil
	default:
		return 0, fmt.Errorf("unknown format %q; want table, csv or json", name)
	}
}

// render writes a result set and reports how many rows it held.
func (f format) render(w io.Writer, rows *sql.Rows) (int, error) {
	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	types, err := rows.ColumnTypes()
	if err != nil {
		return 0, err
	}

	// The whole result is read before anything is written, because the table
	// form cannot know how wide a column is until it has seen the widest value
	// in it. At the scale lightsql targets that is a fair trade for output that
	// lines up; a streaming form would have to guess and then be wrong.
	//
	// The values are kept as they came back rather than as text, because JSON
	// needs the types: rendering first and encoding second would turn every
	// number into a string and every NULL into the word.
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return 0, err
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	switch f {
	case formatCSV:
		return len(out), writeCSV(w, cols, out)
	case formatJSON:
		return len(out), writeJSON(w, cols, out)
	default:
		return len(out), writeTable(w, cols, types, out)
	}
}

// countsRows reports whether the format should be followed by a row count. The
// machine-readable ones must not be: a consumer parsing CSV would have to know
// to drop a trailing line that is not CSV.
func (f format) countsRows() bool { return f == formatTable }

// text renders a row for the forms that are text all the way down.
func text(row []any) []string {
	out := make([]string, len(row))
	for i, v := range row {
		out[i] = display(v)
	}
	return out
}

// display renders one value the way SQL writes it.
//
// NULL is spelled out rather than left blank, so that it cannot be mistaken for
// an empty string -- which is exactly the confusion the engine's value model
// exists to prevent, and it would be a pity to reintroduce it at the last step.
// That holds for CSV too, where the convention is an empty field: it trades one
// ambiguity for a worse one, since empty-versus-NULL is the distinction someone
// reading the output most often needs. Anything that needs it mechanically
// should ask for JSON, which has a real null.
func display(v any) string {
	switch v := v.(type) {
	case nil:
		return "NULL"
	case string:
		return v
	case []byte:
		// bytea, written the way PostgreSQL writes it.
		return `\x` + hex.EncodeToString(v)
	case bool:
		return strconv.FormatBool(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case time.Time:
		return v.Format("2006-01-02 15:04:05.999999-07:00")
	default:
		return fmt.Sprint(v)
	}
}

// numericColumn reports whether a column should be right-aligned, which is
// decided from the column's declared type rather than from what the values
// happen to look like -- a text column full of digits is still text.
func numericColumn(t *sql.ColumnType) bool {
	switch strings.ToLower(t.DatabaseTypeName()) {
	case "smallint", "integer", "bigint", "real", "double precision", "numeric":
		return true
	default:
		return false
	}
}

func writeTable(w io.Writer, cols []string, types []*sql.ColumnType, raw [][]any) error {
	rows := make([][]string, len(raw))
	for i, r := range raw {
		rows[i] = text(r)
	}

	width := make([]int, len(cols))
	for i, c := range cols {
		width[i] = len([]rune(c))
	}
	for _, r := range rows {
		for i, cell := range r {
			if n := len([]rune(cell)); n > width[i] {
				width[i] = n
			}
		}
	}

	var sb strings.Builder
	for i, c := range cols {
		if i > 0 {
			sb.WriteString(" | ")
		}
		sb.WriteString(pad(c, width[i], false))
	}
	sb.WriteString("\n")
	for i := range cols {
		if i > 0 {
			sb.WriteString("-+-")
		}
		sb.WriteString(strings.Repeat("-", width[i]))
	}
	sb.WriteString("\n")

	for _, r := range rows {
		for i, cell := range r {
			if i > 0 {
				sb.WriteString(" | ")
			}
			// Alignment follows the column, not the cell, so a NULL in a
			// numeric column does not step out of the stack of figures.
			right := i < len(types) && numericColumn(types[i])
			sb.WriteString(pad(cell, width[i], right))
		}
		sb.WriteString("\n")
	}
	_, err := io.WriteString(w, sb.String())
	return err
}

// pad widens a cell to n runes, counting runes rather than bytes so that a
// column holding anything outside ASCII still lines up.
func pad(s string, n int, right bool) string {
	gap := n - len([]rune(s))
	if gap <= 0 {
		return s
	}
	if right {
		return strings.Repeat(" ", gap) + s
	}
	return s + strings.Repeat(" ", gap)
}

func writeCSV(w io.Writer, cols []string, rows [][]any) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(cols); err != nil {
		return err
	}
	for _, r := range rows {
		if err := cw.Write(text(r)); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// writeJSON keeps the types: a number stays a number and NULL becomes null,
// so that whatever is on the other end of the pipe does not have to guess which
// strings were meant to be values.
func writeJSON(w io.Writer, cols []string, rows [][]any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		obj := make(map[string]any, len(cols))
		for i, c := range cols {
			obj[c] = jsonValue(r[i])
		}
		out = append(out, obj)
	}
	return enc.Encode(out)
}

// jsonValue maps a scanned value onto a JSON one.
//
// A timestamp and a byte string both become strings, since JSON has no type for
// either; everything else has a direct counterpart, and using it is the whole
// point of choosing JSON over CSV.
func jsonValue(v any) any {
	switch v := v.(type) {
	case nil, bool, int64, float64, string:
		return v
	case []byte:
		return `\x` + hex.EncodeToString(v)
	case time.Time:
		return v.Format(time.RFC3339Nano)
	default:
		return fmt.Sprint(v)
	}
}
