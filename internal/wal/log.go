package wal

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/oxisto/lightsql/internal/pgerr"
)

// Name is the file the log lives in, inside the database directory.
const Name = "wal"

// magic identifies the file and its format version. A file that does not start
// with it is refused rather than treated as an empty log, so pointing lightsql
// at the wrong path reports an error instead of appearing to work and then
// overwriting whatever was there.
var magic = [8]byte{'L', 'S', 'Q', 'L', 'W', 'A', 'L', 1}

// frameHeader is the four-byte payload length followed by the four-byte
// checksum of that payload.
const frameHeader = 8

// castagnoli is used rather than the IEEE polynomial because it is the one with
// hardware support on the architectures lightsql runs on, and the checksum is
// computed over every byte written.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Log is an append-only file of committed changes.
//
// One frame holds one transaction. That is deliberate: a torn write leaves a
// frame whose length or checksum does not match, and recovery discards the
// whole frame, so a transaction is either replayed entirely or not at all. A
// per-record framing would need a separate commit marker and the discipline to
// never treat records before it as applied.
type Log struct {
	dir   string
	fsync bool

	mu sync.Mutex
	f  *os.File
	// replayed guards the ordering below: nothing may be appended until the
	// existing contents have been read and any partial tail removed, or a good
	// frame would be written after a bad one and become unreachable.
	replayed bool
	// buf is reused between writes. A commit encodes into it and then goes to
	// the file in one call, so a partially encoded transaction never reaches
	// the disk.
	buf []byte
}

// Open opens the log in dir, creating the directory and the file if they do not
// exist. Nothing is appended until Replay has run.
//
// When sync is set every commit is flushed to the platter before it is reported
// as committed. That is the only setting under which a crash cannot lose an
// acknowledged transaction, and it is why it is the default for a file-backed
// database; turning it off trades that guarantee for speed.
func Open(dir string, fsync bool) (*Log, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, pgerr.Newf(pgerr.InternalError, "creating database directory: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, Name), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, pgerr.Newf(pgerr.InternalError, "opening write-ahead log: %v", err)
	}

	l := &Log{dir: dir, fsync: fsync, f: f}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, l.fail("sizing write-ahead log", err)
	}
	if size == 0 {
		if err := l.writeAt(0, magic[:]); err != nil {
			return nil, l.fail("writing log header", err)
		}
		return l, nil
	}

	var head [len(magic)]byte
	if _, err := f.ReadAt(head[:], 0); err != nil || head != magic {
		_ = f.Close()
		return nil, pgerr.Newf(pgerr.DataCorrupted,
			"%s is not a lightsql database directory", dir)
	}
	return l, nil
}

// Replay hands every complete transaction to fn, oldest first, and then removes
// anything after the last complete one.
//
// Truncating is the point. A crash during a commit leaves a partial frame at
// the end of the file; leaving it there would mean the next append landed after
// garbage, and every later transaction would be unreachable on the following
// restart. Recovery is therefore also a repair.
func (l *Log) Replay(fn func([]Record) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.replayed {
		return pgerr.New(pgerr.InternalError, "write-ahead log has already been replayed")
	}

	data, err := os.ReadFile(filepath.Join(l.dir, Name))
	if err != nil {
		return pgerr.Newf(pgerr.InternalError, "reading write-ahead log: %v", err)
	}
	rest := data[len(magic):]
	good := int64(len(magic))

	for {
		recs, remaining, err := nextFrame(rest)
		if err != nil {
			// A frame that does not decode ends the log. It cannot be skipped:
			// the length that says where the next frame starts is part of what
			// is in doubt, so everything after it is unreadable too.
			break
		}
		if recs == nil {
			break
		}
		if err := fn(recs); err != nil {
			return err
		}
		good += int64(len(rest) - len(remaining))
		rest = remaining
	}

	if good != int64(len(data)) {
		if err := l.f.Truncate(good); err != nil {
			return l.fail("truncating a partial transaction", err)
		}
	}
	if _, err := l.f.Seek(good, io.SeekStart); err != nil {
		return l.fail("positioning write-ahead log", err)
	}
	l.replayed = true
	return nil
}

// nextFrame decodes one frame, returning nil records when src is exhausted.
func nextFrame(src []byte) ([]Record, []byte, error) {
	if len(src) == 0 {
		return nil, src, nil
	}
	if len(src) < frameHeader {
		return nil, nil, errTruncated
	}
	n := binary.LittleEndian.Uint32(src)
	sum := binary.LittleEndian.Uint32(src[4:])
	if uint64(n) > maxLen {
		return nil, nil, pgerr.Newf(pgerr.DataCorrupted, "transaction length %d is out of range", n)
	}
	body := src[frameHeader:]
	if uint32(len(body)) < n {
		return nil, nil, errTruncated
	}
	body, rest := body[:n], body[n:]
	if crc32.Checksum(body, castagnoli) != sum {
		return nil, nil, pgerr.New(pgerr.DataCorrupted, "transaction checksum does not match")
	}

	count, body, err := decodeUvarint(body)
	if err != nil {
		return nil, nil, err
	}
	if count == 0 || count > maxLen {
		return nil, nil, pgerr.Newf(pgerr.DataCorrupted, "transaction holds %d records", count)
	}
	recs := make([]Record, count)
	for i := range recs {
		if recs[i], body, err = decodeRecord(body); err != nil {
			return nil, nil, err
		}
	}
	if len(body) != 0 {
		return nil, nil, pgerr.Newf(pgerr.DataCorrupted, "transaction has %d trailing bytes", len(body))
	}
	return recs, rest, nil
}

// Write appends one transaction's records and, when syncing is on, waits for
// them to reach the disk.
//
// It returns before the caller marks the transaction committed, so a
// transaction that cannot be logged is not reported as committed either. Doing
// it the other way round is how a database acknowledges work it then loses.
func (l *Log) Write(recs []Record) error {
	if len(recs) == 0 {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.replayed {
		return pgerr.New(pgerr.InternalError, "write-ahead log has not been replayed")
	}
	if _, err := l.f.Write(l.encode(recs)); err != nil {
		return l.fail("appending to write-ahead log", err)
	}
	if l.fsync {
		if err := l.f.Sync(); err != nil {
			return l.fail("flushing write-ahead log", err)
		}
	}
	return nil
}

// encode lays out one frame in the reusable buffer.
func (l *Log) encode(recs []Record) []byte {
	l.buf = l.buf[:0]
	l.buf = append(l.buf, 0, 0, 0, 0, 0, 0, 0, 0) // the header, filled in below
	l.buf = binary.AppendUvarint(l.buf, uint64(len(recs)))
	for i := range recs {
		l.buf = appendRecord(l.buf, &recs[i])
	}
	body := l.buf[frameHeader:]
	binary.LittleEndian.PutUint32(l.buf, uint32(len(body)))
	binary.LittleEndian.PutUint32(l.buf[4:], crc32.Checksum(body, castagnoli))
	return l.buf
}

// Checkpoint replaces the log with recs, which must describe the current state
// of the database rather than the history that produced it.
//
// Without this the log grows for as long as the database is written to, and
// startup replays every change ever made rather than the rows that survived
// them. The replacement is written to a second file and renamed over the first,
// so a crash during a checkpoint leaves the old log intact: at no point is
// there a moment where neither file describes a complete database.
func (l *Log) Checkpoint(recs []Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.replayed {
		return pgerr.New(pgerr.InternalError, "write-ahead log has not been replayed")
	}

	tmp := filepath.Join(l.dir, Name+".tmp")
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return pgerr.Newf(pgerr.InternalError, "creating checkpoint: %v", err)
	}
	if err := writeCheckpoint(f, l.encode, recs); err != nil {
		_, _ = f.Close(), os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return pgerr.Newf(pgerr.InternalError, "closing checkpoint: %v", err)
	}
	if err := os.Rename(tmp, filepath.Join(l.dir, Name)); err != nil {
		_ = os.Remove(tmp)
		return pgerr.Newf(pgerr.InternalError, "installing checkpoint: %v", err)
	}
	if err := syncDir(l.dir); err != nil {
		return err
	}

	// Reopen so that later appends go to the file the rename installed rather
	// than to the unlinked one this handle still refers to.
	if err := l.f.Close(); err != nil {
		return pgerr.Newf(pgerr.InternalError, "closing write-ahead log: %v", err)
	}
	l.f, err = os.OpenFile(filepath.Join(l.dir, Name), os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return pgerr.Newf(pgerr.InternalError, "reopening write-ahead log: %v", err)
	}
	return nil
}

// writeCheckpoint fills a fresh file with the header and, when there is
// anything to say, one frame holding the whole state.
func writeCheckpoint(f *os.File, encode func([]Record) []byte, recs []Record) error {
	if _, err := f.Write(magic[:]); err != nil {
		return pgerr.Newf(pgerr.InternalError, "writing checkpoint header: %v", err)
	}
	if len(recs) > 0 {
		if _, err := f.Write(encode(recs)); err != nil {
			return pgerr.Newf(pgerr.InternalError, "writing checkpoint: %v", err)
		}
	}
	if err := f.Sync(); err != nil {
		return pgerr.Newf(pgerr.InternalError, "flushing checkpoint: %v", err)
	}
	return nil
}

// Close flushes and releases the file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.f == nil {
		return nil
	}
	err := l.f.Sync()
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	if err != nil {
		return pgerr.Newf(pgerr.InternalError, "closing write-ahead log: %v", err)
	}
	return nil
}

// writeAt writes at a fixed offset and leaves the file positioned after it,
// used only for the header of a fresh log.
func (l *Log) writeAt(off int64, b []byte) error {
	if _, err := l.f.WriteAt(b, off); err != nil {
		return err
	}
	_, err := l.f.Seek(off+int64(len(b)), io.SeekStart)
	return err
}

// fail closes the file and wraps err, so that a log which has hit an I/O error
// cannot go on being appended to as though nothing happened.
func (l *Log) fail(what string, err error) error {
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
	return pgerr.Newf(pgerr.InternalError, "%s: %v", what, err)
}

// syncDir flushes the directory entry, without which a crash just after a
// rename can leave the checkpoint written but not yet named.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return pgerr.Newf(pgerr.InternalError, "opening database directory: %v", err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return pgerr.Newf(pgerr.InternalError, "flushing database directory: %v", err)
	}
	return nil
}
