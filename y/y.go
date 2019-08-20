/*
 * Copyright 2017 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package y

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"reflect"
	"sync"
	"time"
	"unsafe"

	"github.com/pkg/errors"
)

// ErrEOF indicates an end of file when trying to read from a memory mapped file
// and encountering the end of slice.
var ErrEOF = errors.New("End of mapped region")

const (
	// Sync indicates that O_DSYNC should be set on the underlying file,
	// ensuring that data writes do not return until the data is flushed
	// to disk.
	Sync = 1 << iota
	// ReadOnly opens the underlying file on a read-only basis.
	ReadOnly
)

var (
	// This is O_DSYNC (datasync) on platforms that support it -- see file_unix.go
	datasyncFileFlag = 0x0

	// CastagnoliCrcTable is a CRC32 polynomial table
	CastagnoliCrcTable = crc32.MakeTable(crc32.Castagnoli)

	// Dummy channel for nil closers.
	dummyCloserChan = make(chan struct{})
)

// OpenExistingFile opens an existing file, errors if it doesn't exist.
func OpenExistingFile(filename string, flags uint32) (*os.File, error) {
	openFlags := os.O_RDWR
	if flags&ReadOnly != 0 {
		openFlags = os.O_RDONLY
	}

	if flags&Sync != 0 {
		openFlags |= datasyncFileFlag
	}
	return os.OpenFile(filename, openFlags, 0)
}

// CreateSyncedFile creates a new file (using O_EXCL), errors if it already existed.
func CreateSyncedFile(filename string, sync bool) (*os.File, error) {
	flags := os.O_RDWR | os.O_CREATE | os.O_EXCL
	if sync {
		flags |= datasyncFileFlag
	}
	return os.OpenFile(filename, flags, 0666)
}

// OpenSyncedFile creates the file if one doesn't exist.
func OpenSyncedFile(filename string, sync bool) (*os.File, error) {
	flags := os.O_RDWR | os.O_CREATE
	if sync {
		flags |= datasyncFileFlag
	}
	return os.OpenFile(filename, flags, 0666)
}

// OpenTruncFile opens the file with O_RDWR | O_CREATE | O_TRUNC
func OpenTruncFile(filename string, sync bool) (*os.File, error) {
	flags := os.O_RDWR | os.O_CREATE | os.O_TRUNC
	if sync {
		flags |= datasyncFileFlag
	}
	return os.OpenFile(filename, flags, 0666)
}

// SafeCopy does append(a[:0], src...).
func SafeCopy(a, src []byte) []byte {
	return append(a[:0], src...)
}

// Copy copies a byte slice and returns the copied slice.
func Copy(a []byte) []byte {
	b := make([]byte, len(a))
	copy(b, a)
	return b
}

// KeyWithTs generates a new key by appending ts to key.
func KeyWithTs(key []byte, ts uint64) []byte {
	out := make([]byte, len(key)+8)
	copy(out, key)
	binary.BigEndian.PutUint64(out[len(key):], math.MaxUint64-ts)
	return out
}

// ParseTs parses the timestamp from the key bytes.
func ParseTs(key []byte) uint64 {
	if len(key) <= 8 {
		return 0
	}
	return math.MaxUint64 - binary.BigEndian.Uint64(key[len(key)-8:])
}

// CompareKeys checks the key without timestamp and checks the timestamp if keyNoTs
// is same.
// a<timestamp> would be sorted higher than aa<timestamp> if we use bytes.compare
// All keys should have timestamp.
func CompareKeys(key1, key2 []byte) int {
	AssertTrue(len(key1) > 8 && len(key2) > 8)
	if cmp := bytes.Compare(key1[:len(key1)-8], key2[:len(key2)-8]); cmp != 0 {
		return cmp
	}
	return bytes.Compare(key1[len(key1)-8:], key2[len(key2)-8:])
}

// ParseKey parses the actual key from the key bytes.
func ParseKey(key []byte) []byte {
	if key == nil {
		return nil
	}

	AssertTrue(len(key) > 8)
	return key[:len(key)-8]
}

// SameKey checks for key equality ignoring the version timestamp suffix.
func SameKey(src, dst []byte) bool {
	if len(src) != len(dst) {
		return false
	}
	return bytes.Equal(ParseKey(src), ParseKey(dst))
}

// Slice holds a reusable buf, will reallocate if you request a larger size than ever before.
// One problem is with n distinct sizes in random order it'll reallocate log(n) times.
type Slice struct {
	buf []byte
}

// Resize reuses the Slice's buffer (or makes a new one) and returns a slice in that buffer of
// length sz.
func (s *Slice) Resize(sz int) []byte {
	if cap(s.buf) < sz {
		s.buf = make([]byte, sz)
	}
	return s.buf[0:sz]
}

// FixedDuration returns a string representation of the given duration with the
// hours, minutes, and seconds.
func FixedDuration(d time.Duration) string {
	str := fmt.Sprintf("%02ds", int(d.Seconds())%60)
	if d >= time.Minute {
		str = fmt.Sprintf("%02dm", int(d.Minutes())%60) + str
	}
	if d >= time.Hour {
		str = fmt.Sprintf("%02dh", int(d.Hours())) + str
	}
	return str
}

// Closer holds the two things we need to close a goroutine and wait for it to finish: a chan
// to tell the goroutine to shut down, and a WaitGroup with which to wait for it to finish shutting
// down.
type Closer struct {
	closed  chan struct{}
	waiting sync.WaitGroup
}

// NewCloser constructs a new Closer, with an initial count on the WaitGroup.
func NewCloser(initial int) *Closer {
	ret := &Closer{closed: make(chan struct{})}
	ret.waiting.Add(initial)
	return ret
}

// AddRunning Add()'s delta to the WaitGroup.
func (lc *Closer) AddRunning(delta int) {
	lc.waiting.Add(delta)
}

// Signal signals the HasBeenClosed signal.
func (lc *Closer) Signal() {
	close(lc.closed)
}

// HasBeenClosed gets signaled when Signal() is called.
func (lc *Closer) HasBeenClosed() <-chan struct{} {
	if lc == nil {
		return dummyCloserChan
	}
	return lc.closed
}

// Done calls Done() on the WaitGroup.
func (lc *Closer) Done() {
	if lc == nil {
		return
	}
	lc.waiting.Done()
}

// Wait waits on the WaitGroup.  (It waits for NewCloser's initial value, AddRunning, and Done
// calls to balance out.)
func (lc *Closer) Wait() {
	lc.waiting.Wait()
}

// SignalAndWait calls Signal(), then Wait().
func (lc *Closer) SignalAndWait() {
	lc.Signal()
	lc.Wait()
}

// Throttle allows a limited number of workers to run at a time. It also
// provides a mechanism to check for errors encountered by workers and wait for
// them to finish.
type Throttle struct {
	once      sync.Once
	wg        sync.WaitGroup
	ch        chan struct{}
	errCh     chan error
	finishErr error
}

// NewThrottle creates a new throttle with a max number of workers.
func NewThrottle(max int) *Throttle {
	return &Throttle{
		ch:    make(chan struct{}, max),
		errCh: make(chan error, max),
	}
}

// Do should be called by workers before they start working. It blocks if there
// are already maximum number of workers working. If it detects an error from
// previously Done workers, it would return it.
func (t *Throttle) Do() error {
	for {
		select {
		case t.ch <- struct{}{}:
			t.wg.Add(1)
			return nil
		case err := <-t.errCh:
			if err != nil {
				return err
			}
		}
	}
}

// Done should be called by workers when they finish working. They can also
// pass the error status of work done.
func (t *Throttle) Done(err error) {
	if err != nil {
		t.errCh <- err
	}
	select {
	case <-t.ch:
	default:
		panic("Throttle Do Done mismatch")
	}
	t.wg.Done()
}

// Finish waits until all workers have finished working. It would return any error passed by Done.
// If Finish is called multiple time, it will wait for workers to finish only once(first time).
// From next calls, it will return same error as found on first call.
func (t *Throttle) Finish() error {
	t.once.Do(func() {
		t.wg.Wait()
		close(t.ch)
		close(t.errCh)
		for err := range t.errCh {
			if err != nil {
				t.finishErr = err
				return
			}
		}
	})

	return t.finishErr
}

// U32ToBytes converts the given Uint32 to bytes
func U32ToBytes(v uint32) []byte {
	var uBuf [4]byte
	binary.BigEndian.PutUint32(uBuf[:], v)
	return uBuf[:]
}

// BytesToU32 converts the given byte slice to uint32
func BytesToU32(b []byte) uint32 {
	return binary.BigEndian.Uint32(b)
}

// U32SliceToBytes converts the given Uint32 slice to byte slice
func U32SliceToBytes(u32s []uint32) []byte {
	if len(u32s) == 0 {
		return nil
	}
	var b []byte
	hdr := (*reflect.SliceHeader)(unsafe.Pointer(&b))
	hdr.Len = len(u32s) * 4
	hdr.Cap = hdr.Len
	hdr.Data = uintptr(unsafe.Pointer(&u32s[0]))
	return b
}

// BytesToU32Slice converts the given byte slice to uint32 slice
func BytesToU32Slice(b []byte) []uint32 {
	if len(b) == 0 {
		return nil
	}
	var u32s []uint32
	hdr := (*reflect.SliceHeader)(unsafe.Pointer(&u32s))
	hdr.Len = len(b) / 4
	hdr.Cap = hdr.Len
	hdr.Data = uintptr(unsafe.Pointer(&b[0]))
	return u32s
}

type page struct {
	buf []byte
}

type Buffer struct {
	length      int
	curPageSize int
	pages       []*page
	pbuf        []byte
}

func NewBuffer(pageSize int) *Buffer {
	b := &Buffer{curPageSize: pageSize}
	b.pages = make([]*page, 0)
	b.pages = append(b.pages, &page{buf: make([]byte, 0, b.curPageSize)})
	b.length = 0
	return b
}

func (b *Buffer) Write(data []byte) (int, error) {
	dlen := len(data)
	written := 0
	for {
		cp := b.pages[len(b.pages)-1] // current page
		sz := len(cp.buf)
		n := copy(cp.buf[sz:cap(cp.buf)], data)
		cp.buf = cp.buf[:sz+n]
		written += n
		if len(data) == n {
			break
		}
		data = data[n:]

		b.curPageSize *= 2
		b.pages = append(b.pages, &page{buf: make([]byte, 0, b.curPageSize)})
	}
	b.length += dlen

	return written, nil
}

func (b *Buffer) WriteByte(data byte) {
	b.Write([]byte{data})
}

func (b *Buffer) Len() int {
	return b.length
}

func (b *Buffer) ReadAt(offset, length int) []byte {
	if b.length-offset < length || length == -1 {
		length = b.length - offset
	}

	if length == 0 {
		return nil
	}

	buf := make([]byte, length) // Allocate whole buffer at start.

	var pageIdx, startIdx, sizeNow int
	for i, page := range b.pages {
		if sizeNow+len(page.buf)-1 < offset {
			sizeNow += len(page.buf)
		} else {
			pageIdx = i
			startIdx = offset - sizeNow
		}
	}

	read := 0
	for {
		cp := b.pages[pageIdx]
		read += copy(buf[read:], cp.buf[startIdx:])
		if read >= length {
			break
		}
		startIdx = 0
	}
	return buf
}

func (b *Buffer) Bytes() []byte {
	buf := make([]byte, b.length)
	written := 0
	for i := 0; i < len(b.pages); i++ {
		written += copy(buf[written:], b.pages[i].buf)
	}

	return buf
}

func (b *Buffer) NewReader() io.Reader {
	// Allocates the right slice. Copies over the data and returns.

	return &reader{
		b:        b,
		pageIdx:  0,
		startIdx: 0,
	}
}

func (b *Buffer) WriteTo(w io.Writer) {
	for i := 0; i < len(b.pages); i++ {
		w.Write(b.pages[i].buf[:])
	}
}

// To create hash.
// func (b *Buffer) NewReaderAt(offset, length int) io.Reader {
// 	// Iterates over the pages and writes to io.Writer.
// 	return &reader{b: b, offset: offset, length: length}
// }

type reader struct {
	b *Buffer
	// offset int
	// length int
	pageIdx  int
	startIdx int
}

// // io.Copy(fd, b.NewReader(0, -1))

func (r *reader) Read(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}

	read := 0
	for r.pageIdx < len(r.b.pages) {
		cp := r.b.pages[r.pageIdx]
		n := copy(buf[read:], cp.buf[r.startIdx:])
		read += n
		r.startIdx += n
		if r.startIdx >= len(cp.buf) {
			r.pageIdx++
			r.startIdx = 0
		}
		if read >= len(buf) {
			return read, nil
		}
	}

	if read < len(buf) {
		return read, io.EOF
	}

	return read, nil
}

func (r *reader) WriteTo(w io.Writer) (int64, error) {
	var written int64
	for r.pageIdx < len(r.b.pages) {
		cp := r.b.pages[r.pageIdx]
		n, err := w.Write(cp.buf[r.startIdx:])
		r.startIdx += n
		written += int64(n)
		if err != nil {
			return written, err
		}
		if r.startIdx >= len(cp.buf) {
			r.pageIdx++
			r.startIdx = 0
		}
	}

	return written, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
