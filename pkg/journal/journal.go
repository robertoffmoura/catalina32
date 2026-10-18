// Package journal records the values a live session takes from outside the guest (clock
// readings, keyboard and controller state, audio pacing) so that a later run can replay them.
// Everything else the emulator does is deterministic, so a replay reaches the same game states
// as the session, frame for frame: a recorded playthrough can be rerun headless to inspect a
// scene the scripted routes do not reach.
//
//	C32_RECORD=<dir>  record into <dir> (journal.gz, plus a copy of the save folder as it was)
//	C32_REPLAY=<dir>  replay <dir>: the save copy is restored into an empty save folder
//
// Entries are read back in the order they were written, each tagged with its kind; the first
// mismatch (the replay diverged, e.g. a different build or settings) ends the replay and the
// run continues with live values.
package journal

import (
	"bufio"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Kinds of entries.
const (
	Frame  byte = 'F' // a Present (frame number), the replay's checkpoints
	Millis byte = 'M' // timeGetTime / GetTickCount
	Perf   byte = 'P' // QueryPerformanceCounter
	Keys   byte = 'K' // a keyboard state buffer
	Pad    byte = 'J' // a controller read (state and whether connected)
	Audio  byte = 'A' // audio frames mixed at a Present
)

const magic = "C32J1\n"

// Journal is used from the emulation goroutine only. A nil *Journal passes live values through.
type Journal struct {
	replay  bool
	w       *bufio.Writer
	gz      *gzip.Writer
	f       *os.File
	r       *bufio.Reader
	n       uint64 // entries so far
	frame   uint64 // last frame checkpoint
	scratch []byte
}

// Open starts recording or replaying as C32_RECORD / C32_REPLAY ask (nil when neither is set).
// settings describes the options that change what the guest does; a replay warns when they
// differ from the recording's.
func Open(saveRoot, settings string) *Journal {
	if dir := os.Getenv("C32_RECORD"); dir != "" {
		j, err := record(dir, saveRoot, settings)
		if err != nil {
			fmt.Printf("[journal] cannot record into %s: %v\n", dir, err)
			return nil
		}
		fmt.Printf("[journal] recording this session into %s\n", dir)
		return j
	}
	if dir := os.Getenv("C32_REPLAY"); dir != "" {
		j, err := replay(dir, saveRoot, settings)
		if err != nil {
			fmt.Printf("[journal] cannot replay %s: %v\n", dir, err)
			return nil
		}
		fmt.Printf("[journal] replaying %s\n", dir)
		return j
	}
	return nil
}

func record(dir, saveRoot, settings string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	if err := copyTree(saveRoot, filepath.Join(dir, "save")); err != nil {
		return nil, fmt.Errorf("copying the save folder: %w", err)
	}
	f, err := os.Create(filepath.Join(dir, "journal.gz"))
	if err != nil {
		return nil, err
	}
	gz := gzip.NewWriter(f)
	j := &Journal{f: f, gz: gz, w: bufio.NewWriter(gz)}
	j.w.WriteString(magic)
	j.writeBytes([]byte(settings))
	return j, nil
}

func replay(dir, saveRoot, settings string) (*Journal, error) {
	f, err := os.Open(filepath.Join(dir, "journal.gz"))
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	j := &Journal{replay: true, f: f, r: bufio.NewReader(gz)}
	head := make([]byte, len(magic))
	if _, err := io.ReadFull(j.r, head); err != nil || string(head) != magic {
		return nil, fmt.Errorf("not a journal")
	}
	recorded, err := j.readBytes()
	if err != nil {
		return nil, err
	}
	if string(recorded) != settings {
		fmt.Printf("[journal] warning: recorded with %q, replaying with %q; the replay may diverge\n", recorded, settings)
	}
	entries, _ := os.ReadDir(saveRoot)
	if len(entries) > 0 {
		fmt.Printf("[journal] warning: %s is not empty, so the recording's save folder is not restored\n", saveRoot)
	} else if err := copyTree(filepath.Join(dir, "save"), saveRoot); err != nil {
		return nil, fmt.Errorf("restoring the save folder: %w", err)
	}
	return j, nil
}

// Replaying reports whether values come from the journal.
func (j *Journal) Replaying() bool { return j != nil && j.replay }

// U64 returns live() while recording (and records it) and the recorded value while replaying.
func (j *Journal) U64(kind byte, live func() uint64) uint64 {
	if j == nil {
		return live()
	}
	if j.replay {
		if v, ok := j.next(kind); ok {
			return binary.LittleEndian.Uint64(v)
		}
		return live()
	}
	v := live()
	j.put(kind, binary.LittleEndian.AppendUint64(j.scratch[:0], v))
	return v
}

// Bytes records b while recording, and overwrites b with the recorded bytes while replaying.
func (j *Journal) Bytes(kind byte, b []byte) {
	if j == nil {
		return
	}
	if j.replay {
		if v, ok := j.next(kind); ok {
			copy(b, v)
		}
		return
	}
	j.put(kind, b)
}

// Frame marks a Present. Recording flushes here, so a session that is killed loses at most a
// frame; replaying checks that the frame numbers agree.
func (j *Journal) Frame(n uint64) {
	if j == nil {
		return
	}
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], n)
	if j.replay {
		if v, ok := j.next(Frame); ok && binary.LittleEndian.Uint64(v) != n {
			j.stop(fmt.Sprintf("frame %d where the recording has frame %d", n, binary.LittleEndian.Uint64(v)))
		}
		j.frame = n
		return
	}
	j.put(Frame, b[:])
	j.frame = n
	if j.w.Flush() == nil {
		j.gz.Flush()
	}
}

// Close finishes a recording.
func (j *Journal) Close() {
	if j == nil || j.replay {
		return
	}
	j.w.Flush()
	j.gz.Close()
	j.f.Close()
}

func (j *Journal) put(kind byte, b []byte) {
	j.w.WriteByte(kind)
	j.writeBytes(b)
	j.n++
}

func (j *Journal) writeBytes(b []byte) {
	var l [binary.MaxVarintLen64]byte
	j.w.Write(l[:binary.PutUvarint(l[:], uint64(len(b)))])
	j.w.Write(b)
}

func (j *Journal) readBytes() ([]byte, error) {
	n, err := binary.ReadUvarint(j.r)
	if err != nil {
		return nil, err
	}
	if cap(j.scratch) < int(n) {
		j.scratch = make([]byte, n)
	}
	j.scratch = j.scratch[:n]
	_, err = io.ReadFull(j.r, j.scratch)
	return j.scratch, err
}

// next reads the next entry, which must be of the given kind.
func (j *Journal) next(kind byte) ([]byte, bool) {
	if j.r == nil {
		return nil, false
	}
	k, err := j.r.ReadByte()
	if err == nil && k != kind {
		j.stop(fmt.Sprintf("a %q read where the recording has %q (entry %d)", kind, k, j.n))
		return nil, false
	}
	var v []byte
	if err == nil {
		v, err = j.readBytes()
	}
	if err != nil {
		j.stop("the end of the recording")
		return nil, false
	}
	j.n++
	return v, true
}

func (j *Journal) stop(why string) {
	fmt.Printf("[journal] replay ended after frame %d: %s; continuing with live input\n", j.frame, why)
	j.r = nil
	j.f.Close()
}

// copyTree copies the files under src into dst (src may be missing).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && p == src {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, data, 0644)
	})
}
