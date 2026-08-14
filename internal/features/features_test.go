package features

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/oxisto/lightsql/internal/engine"
	"github.com/oxisto/lightsql/internal/parser"
	"github.com/oxisto/lightsql/internal/types"
)

var update = flag.Bool("update", false, "rewrite README.md from the feature registry")

const (
	readmePath = "../../README.md"
	beginMark  = "<!-- BEGIN GENERATED COMPATIBILITY -->"
	endMark    = "<!-- END GENERATED COMPATIBILITY -->"
	badgeBegin = "<!-- BEGIN GENERATED BADGES -->"
	badgeEnd   = "<!-- END GENERATED BADGES -->"

	// module and goVersion feed the badges. They are constants here rather than
	// read from go.mod because a badge pointing at the wrong module is worse
	// than one that fails this test when the module is renamed.
	module    = "github.com/oxisto/lightsql"
	repo      = "oxisto/lightsql"
	goVersion = "1.26"
)

// TestProbesMatchStatus is what keeps the matrix honest. Every feature claiming
// the front end supports it must have a statement that actually parses, and
// every feature claiming otherwise must have one that does not. Without this,
// the table is a comment: it can say anything.
func TestProbesMatchStatus(t *testing.T) {
	seen := make(map[string]bool)

	for _, g := range Groups {
		for _, f := range g.Features {
			if seen[f.Name] {
				t.Errorf("duplicate feature name %q", f.Name)
			}
			seen[f.Name] = true

			if f.Parse == Partial && f.Note == "" {
				t.Errorf("%s: partial status needs a Note saying what is missing", f.Name)
			}
			if f.SQL == "" {
				// A few entries, such as isolation levels, cannot be shown by a
				// single statement. They are exempt, but nothing else is.
				continue
			}

			_, err := parser.Parse(f.SQL)
			switch f.Parse {
			case Yes, Partial:
				if err != nil {
					t.Errorf("%s: claims the parser supports it, but its probe fails\n  SQL: %s\n  err: %v",
						f.Name, f.SQL, err)
				}
			case Planned, No:
				if err == nil {
					t.Errorf("%s: claims the parser does not support it, but its probe parses\n  SQL: %s\n"+
						"  the registry is now understating what works; raise Parse to yes",
						f.Name, f.SQL)
				}
			}
		}
	}
}

// TestExecStatusIsNotAheadOfParse catches a registry entry that claims the
// executor runs something the parser cannot even read.
func TestExecStatusIsNotAheadOfParse(t *testing.T) {
	for _, g := range Groups {
		for _, f := range g.Features {
			if f.Exec > f.Parse {
				t.Errorf("%s: Exec is %s but Parse is only %s", f.Name, f.Exec, f.Parse)
			}
		}
	}
}

// TestExecProbesMatchStatus does for the Exec column what TestProbesMatchStatus
// does for Parse: it actually runs each probe.
//
// This is the difference between a matrix that documents intent and one that
// documents behaviour. A feature the parser accepts but the binder rejects is
// not usable, and without running the probe that gap is invisible.
func TestExecProbesMatchStatus(t *testing.T) {
	for _, g := range Groups {
		for _, f := range g.Features {
			if f.SQL == "" || f.Parse == No || f.Parse == Planned {
				continue
			}
			t.Run(f.Name, func(t *testing.T) {
				err := runProbe(f)
				switch f.Exec {
				case Yes, Partial:
					if err != nil {
						t.Errorf("claims the executor supports it, but its probe fails\n"+
							"  SQL: %s\n  err: %v", f.SQL, err)
					}
				case Planned, No:
					if err == nil {
						t.Errorf("claims the executor does not support it, but its probe runs\n"+
							"  SQL: %s\n  the registry understates what works; raise Exec", f.SQL)
					}
				}
			})
		}
	}
}

// runProbe executes a feature's setup and probe in a fresh instance, so probes
// cannot interfere with one another.
func runProbe(f Feature) error {
	eng := engine.New()
	ctx := context.Background()

	for _, s := range f.Setup {
		if _, err := eng.ExecBatch(ctx, nil, s, nil); err != nil {
			return fmt.Errorf("setup %q: %w", s, err)
		}
	}

	// A batch cannot be prepared, by design, so it runs through the same path a
	// caller would use for one.
	if stmts, err := parser.Parse(f.SQL); err == nil && len(stmts) > 1 {
		_, err := eng.ExecBatch(ctx, nil, f.SQL, nil)
		return err
	}

	p, err := eng.Prepare(f.SQL)
	if err != nil {
		return err
	}
	// A probe may use placeholders, which have no arguments here; supplying
	// NULLs keeps the shape valid without asserting anything about the result.
	args := make([]types.Value, p.Params)
	for i := range args {
		args[i] = types.Null()
	}

	if !p.ReturnsRows() {
		_, err := p.Exec(ctx, nil, args)
		return err
	}
	rows, err := p.Query(ctx, nil, args)
	if err != nil {
		return err
	}
	defer rows.Close()

	// A probe runs against a tiny fixture, so a large number of rows means an
	// operator is not terminating. Bounding the loop turns that into a failed
	// test rather than a hung one — which is how an operator that forgot to
	// record that it had already yielded its only row was found.
	const maxProbeRows = 1000
	for n := 0; ; n++ {
		if n > maxProbeRows {
			return fmt.Errorf("probe produced more than %d rows; an operator is not terminating", maxProbeRows)
		}
		_, ok, err := rows.Next(ctx)
		if err != nil || !ok {
			return err
		}
	}
}

// TestREADMEIsGenerated fails when README.md has drifted from the registry.
// Run `go test ./internal/features -update` to regenerate it.
func TestREADMEIsGenerated(t *testing.T) {
	current, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("reading README: %v", err)
	}

	want, err := replaceSection(string(current), beginMark, endMark, "\n\n"+Markdown()+"\n")
	if err != nil {
		t.Fatal(err)
	}
	want, err = replaceSection(want, badgeBegin, badgeEnd, "\n"+Badges(module, repo, goVersion))
	if err != nil {
		t.Fatal(err)
	}

	if string(current) == want {
		return
	}
	if *update {
		if err := os.WriteFile(readmePath, []byte(want), 0o644); err != nil {
			t.Fatalf("writing README: %v", err)
		}
		t.Log("README.md regenerated")
		return
	}
	t.Errorf("README.md is out of date with the feature registry.\n" +
		"Run: go test ./internal/features -update")
}

// replaceSection swaps the content between the generated-section markers,
// leaving the rest of the file alone so the README stays hand-written apart
// from the matrix.
func replaceSection(doc, begin, end, section string) (string, error) {
	i := strings.Index(doc, begin)
	if i < 0 {
		return "", errMissingMarker(begin)
	}
	j := strings.Index(doc, end)
	if j < 0 || j < i {
		return "", errMissingMarker(end)
	}
	return doc[:i+len(begin)] + section + doc[j:], nil
}

type errMissingMarker string

func (e errMissingMarker) Error() string {
	return "README.md is missing the marker " + string(e)
}
