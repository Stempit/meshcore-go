package meshcore

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// fakeStream is an in-memory io.ReadWriteCloser: Read consumes bytes pushed
// into in, Write collects everything written, and Close unblocks a pending
// Read like a real socket or serial port does.
type fakeStream struct {
	mu        sync.Mutex
	written   []byte
	readErr   error
	in        chan byte
	closedCh  chan struct{}
	closeOnce sync.Once
}

func newFakeStream() *fakeStream {
	return &fakeStream{in: make(chan byte, 256), closedCh: make(chan struct{})}
}

func (f *fakeStream) Read(p []byte) (int, error) {
	select {
	case b, ok := <-f.in:
		if !ok {
			return 0, f.readErr
		}
		p[0] = b
		return 1, nil
	case <-f.closedCh:
		return 0, io.ErrClosedPipe
	}
}

func (f *fakeStream) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-f.closedCh:
		return 0, io.ErrClosedPipe
	default:
	}
	f.written = append(f.written, p...)
	return len(p), nil
}

func (f *fakeStream) Close() error {
	f.closeOnce.Do(func() { close(f.closedCh) })
	return nil
}

// push queues bytes to be delivered by subsequent Read calls.
func (f *fakeStream) push(bs ...byte) {
	for _, b := range bs {
		f.in <- b
	}
}

// endWithEOF closes the input so Read returns readErr (io.EOF by default).
func (f *fakeStream) endWith(err error) {
	f.mu.Lock()
	f.readErr = err
	f.mu.Unlock()
	close(f.in)
}

func (f *fakeStream) writtenBytes() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.written...)
}

// newTestTransport starts a streamTransport over fs and registers cleanup.
func newTestTransport(t *testing.T, fs *fakeStream) (*streamTransport, chan []byte, chan error) {
	t.Helper()
	frames := make(chan []byte, 16)
	lost := make(chan error, 1)
	tr := newStreamTransport(fs,
		func(b []byte) { frames <- append([]byte(nil), b...) },
		func(err error) { lost <- err },
		log(),
	)
	t.Cleanup(func() { _ = tr.Close() })
	return tr, frames, lost
}

func expectFrame(t *testing.T, frames chan []byte, want []byte) {
	t.Helper()
	select {
	case got := <-frames:
		if string(got) != string(want) {
			t.Fatalf("got frame %x, want %x", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for frame %x", want)
	}
}

func TestStreamTransportWriteFrame(t *testing.T) {
	fs := newFakeStream()
	tr, _, _ := newTestTransport(t, fs)

	if err := tr.WriteFrame([]byte{cmdDeviceQuery, appProtocolVersion}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}

	want := []byte{'<', 0x02, 0x00, cmdDeviceQuery, appProtocolVersion}
	if got := fs.writtenBytes(); string(got) != string(want) {
		t.Fatalf("got written %x, want %x", got, want)
	}
}

func TestStreamTransportWriteFrameTooLarge(t *testing.T) {
	fs := newFakeStream()
	tr, _, _ := newTestTransport(t, fs)

	// The firmware rejects frames larger than MAX_FRAME_SIZE; the client
	// must fail fast and write nothing.
	if err := tr.WriteFrame(make([]byte, maxWriteFrameLen+1)); err == nil {
		t.Fatal("expected error for oversized frame")
	}
	if got := fs.writtenBytes(); len(got) != 0 {
		t.Fatalf("oversized frame was written: %x", got)
	}
}

func TestStreamTransportReadLoop(t *testing.T) {
	fs := newFakeStream()
	_, frames, _ := newTestTransport(t, fs)

	// Two frames with one garbage byte between them, split across pushes to
	// exercise the byte-at-a-time parser and inter-frame resync.
	fs.push('>', 0x01, 0x00, respCodeOk)
	fs.push(0x2A, '>', 0x02, 0x00, respCodeDeviceInfo, 14)

	expectFrame(t, frames, []byte{respCodeOk})
	expectFrame(t, frames, []byte{respCodeDeviceInfo, 14})
}

func TestStreamTransportInvalidLengthResets(t *testing.T) {
	fs := newFakeStream()
	_, frames, _ := newTestTransport(t, fs)

	// 0xFFFF exceeds maxStreamFrameLen: the parser must resync instead of
	// swallowing the following valid frame.
	fs.push('>', 0xFF, 0xFF)
	fs.push('>', 0x01, 0x00, respCodeOk)

	expectFrame(t, frames, []byte{respCodeOk})
}

func TestStreamTransportEOFSignalsLost(t *testing.T) {
	fs := newFakeStream()
	tr, _, lost := newTestTransport(t, fs)

	fs.endWith(io.EOF)

	select {
	case err := <-lost:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("onLost got %v, want io.EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for onLost")
	}

	// The read loop must have exited.
	select {
	case <-tr.doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("read loop still running after EOF")
	}
}

func TestStreamTransportCloseUnblocksReadLoop(t *testing.T) {
	fs := newFakeStream() // no data: the read loop blocks in Read
	tr, _, lost := newTestTransport(t, fs)

	done := make(chan struct{})
	go func() {
		_ = tr.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return while the read loop was blocked")
	}

	select {
	case err := <-lost:
		t.Fatalf("onLost called on deliberate Close: %v", err)
	default:
	}
}
