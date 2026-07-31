package net

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliecharlieO-o/ridedaemon-go/hud/stream"
	"github.com/charliecharlieO-o/ridedaemon-go/internal/logging"
)

type connectionState struct {
	frameCounter uint32
	pollCount    uint64
}

// buildFramedPacket wraps one access unit for the dash's data socket.
//
// withIndex (the only format MOTO-HUB has ever streamed):
//
//	4B length (index + AU) | 4B frame index | Annex-B AU
//
// Without it, the format the EasyConn reverse-engineering notes document instead:
//
//	4B length (AU) | Annex-B AU
//
// A dash that scans for the 00 00 00 01 start code swallows the extra index either
// way, which is why every unit driven so far works with the indexed form. One that
// hands the buffer straight to its decoder does not. See
// [MediaStream.SetPlainFramingAllowed] for who gets to choose.
func buildFramedPacket(body []byte, frameCounter uint32, withIndex bool) ([]byte, uint32) {
	idx := frameCounter

	payloadLen := len(body)
	if withIndex {
		payloadLen += 4
	}

	// 4B len
	lenBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(lenBytes, uint32(payloadLen))

	// data
	frame := make([]byte, 0, mediaStepFrameSize+len(body))
	frame = append(frame, lenBytes...)
	if withIndex {
		// 4B index
		idxBytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(idxBytes, idx)
		frame = append(frame, idxBytes...)
	}
	frame = append(frame, body...)

	return frame, idx
}

func sendChunked(w *bufio.Writer, frame []byte, chunkSize int, sleep time.Duration) error {
	// no chunking needed
	if chunkSize <= 0 {
		if _, err := w.Write(frame); err != nil {
			return err
		}
		return w.Flush()
	}

	offset := 0
	for offset < len(frame) {
		end := offset + chunkSize
		if end > len(frame) {
			end = len(frame)
		}

		// Write chunk :p
		if _, err := w.Write(frame[offset:end]); err != nil {
			return err
		}
		if err := w.Flush(); err != nil {
			return err
		}

		offset = end
		if sleep > 0 {
			time.Sleep(sleep)
		}
	}
	return nil
}

// Step frame 72 00 00 00 00 00 00 00 - 8 bytes
const mediaStepFrameSize = 8

type MediaStream struct {
	port     string
	quit     chan any
	wg       sync.WaitGroup
	listener net.Listener
	tracker  *ConnTracker

	stopOnce sync.Once

	// Shared frame source (temp)
	src stream.FrameSource

	// Config
	chunkSize  int           // e.g 0x1000
	chunkSleep time.Duration // e.g 3 * time.Millisecond

	// Frame format. plainFramingAllowed is set by the host before the session
	// starts; plainFraming is what the dash actually negotiated. Both default to
	// false, i.e. the indexed format every working unit has been streamed so far.
	plainFramingAllowed atomic.Bool
	plainFraming        atomic.Bool

	// Interface events
	Errors chan error
}

// SetPlainFramingAllowed lets the dash's own supportExtendProtocol byte select the
// un-indexed frame format. Off unless the host asks for it, so a dash it recognises
// keeps the exact bytes on the wire it gets today whatever it reports. Configure it
// before Start.
func (s *MediaStream) SetPlainFramingAllowed(allowed bool) {
	s.plainFramingAllowed.Store(allowed)
}

// NegotiatedExtendedProtocol reports the supportExtendProtocol byte echoed in the
// capture-config reply. It only switches the format when the host allowed it, and
// returns whether plain (un-indexed) framing is in effect after the call — the
// caller forwards that to the phone so the decision is visible in field logs, not
// just in this process's own logging.
func (s *MediaStream) NegotiatedExtendedProtocol(extended bool) bool {
	if !s.plainFramingAllowed.Load() {
		return false
	}
	plain := !extended
	if s.plainFraming.Swap(plain) != plain {
		if plain {
			logging.Printf("MediaStream: dash reports supportExtendProtocol=0, dropping the frame index")
		} else {
			logging.Printf("MediaStream: dash reports supportExtendProtocol=1, keeping the frame index")
		}
	}
	return plain
}

func NewMediaStream(port string, src stream.FrameSource, chunkSize int, chunkSleep time.Duration) *MediaStream {
	return &MediaStream{
		port:       port,
		quit:       make(chan any),
		tracker:    NewConnTracker(),
		Errors:     make(chan error, 16),
		src:        src,
		chunkSize:  chunkSize,
		chunkSleep: chunkSleep,
	}
}

func (s *MediaStream) emitError(err error) {
	select {
	case s.Errors <- err:
	default:
		logging.Printf("Dropping error from MediaStream [No one's listening!]: %v", err)
	}
}

func (s *MediaStream) acceptLoop() {
	defer s.wg.Done()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.quit:
				return // Normal shut down
			default:
				logging.Printf("Error accepting connection: %s", err)
				continue
			}
		}

		logging.Printf("New MediaStream client from %s", conn.RemoteAddr())
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

// Handling individual TCP Connection/Client
func (s *MediaStream) handleConn(conn net.Conn) {
	s.tracker.Add(conn)
	defer func() {
		s.tracker.Remove(conn)
		_ = conn.Close()
		s.wg.Done()
	}()

	// Keep a fast & stead connection
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
		_ = tcpConn.SetKeepAlive(true)
	}

	input := bufio.NewReaderSize(conn, 8*1024)
	output := bufio.NewWriterSize(conn, 64*1024)
	defer func() {
		if err := output.Flush(); err != nil {
			logging.Printf("MediaStream: Error flushing output: %v", err)
		}
	}()

	header := make([]byte, mediaStepFrameSize)
	zero4 := []byte{0, 0, 0, 0}

	st := &connectionState{
		frameCounter: 0,
		pollCount:    0,
	}

	logging.Printf("Starting MediaStream Loop")
	defer logging.Printf("Stopping MediaStream Loop")
	for {
		// Read 8 bytes (poll header)
		if _, err := io.ReadFull(input, header); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				s.emitError(&StrmError{
					StrmDecodeErr,
					fmt.Errorf("error reading header: %v", err),
					true,
				})
				return
			}
			s.emitError(&StrmError{
				StrmDecodeErr,
				fmt.Errorf("unknown error reading header: %v", err),
				true,
			})
			return
		}

		cmd := binary.LittleEndian.Uint16(header[0:2])

		if cmd != 0x0072 {
			// Not a poll pacing command, discard and send idle 0's
			s.emitError(&StrmError{
				StrmUnknownCommandErr,
				fmt.Errorf("non-0x0072 cmd %04x, sending idle", cmd),
				false,
			})
			if _, err := output.Write(zero4); err != nil {
				s.emitError(&StrmError{
					StrmWriteErr,
					fmt.Errorf("error writing idle: %v", err),
					true,
				})
				return
			}
			if err := output.Flush(); err != nil {
				s.emitError(&StrmError{
					StrmWriteErr,
					fmt.Errorf("error flushing idle: %v", err),
					true,
				})
				return
			}
			continue
		}

		st.pollCount++

		body, err := s.src.NextFrame(time.Now())
		if err != nil {
			s.emitError(&StrmError{StrmUnknownCommandErr, fmt.Errorf("error reading frame: %v", err), false})
			// we will send an idle 0s body so the connection stays open
			if _, err = output.Write(zero4); err != nil {
				s.emitError(&StrmError{
					StrmWriteErr,
					fmt.Errorf("MediaStream: error writing idle after src failure: %v", err),
					true,
				})
				return
			}
			if err = output.Flush(); err != nil {
				s.emitError(&StrmError{
					StrmWriteErr,
					fmt.Errorf("MediaStream: error flushing idle after src failure: %v", err),
					true,
				})
				return
			}
			continue
		}

		// If no payload is available, send idle 0s
		if body == nil || len(body) == 0 {
			if _, err = output.Write(zero4); err != nil {
				s.emitError(&StrmError{
					StrmWriteErr,
					fmt.Errorf("error writing idle: %v", err),
					true,
				})
				return
			}
			if err = output.Flush(); err != nil {
				s.emitError(&StrmError{
					StrmWriteErr,
					fmt.Errorf("flush error (idle): %v", err),
					true,
				})
				return
			}
			continue
		}

		// Legacy pacing - 4B len + 4B idx + body
		frame, idx := buildFramedPacket(body, st.frameCounter, !s.plainFraming.Load())
		st.frameCounter = (st.frameCounter + 1) & 0x7FFFFFFF

		// Send it chunked, send it paced
		if err = sendChunked(output, frame, s.chunkSize, s.chunkSleep); err != nil {
			s.emitError(&StrmError{
				StrmWriteErr,
				fmt.Errorf("error sending chunks (idx=%d): %v", idx, err),
				true,
			})
			return
		}
	}
}

func (s *MediaStream) Start() error {
	// Create listener
	ln, err := net.Listen("tcp", s.port)
	if err != nil {
		return err
	}

	s.listener = ln
	s.wg.Add(1)
	go s.acceptLoop()
	return nil
}

func (s *MediaStream) Stop(ctx context.Context) error {
	logging.Printf("Stopping media stream")
	s.stopOnce.Do(func() {
		close(s.quit)
		// Close listener
		if s.listener != nil {
			if err := s.listener.Close(); err != nil {
				logging.Printf("[TCPService] error closing listener: %v", err)
			}
		}
	})

	// Close all open TCP connections
	s.tracker.CloseAll()

	// Wait for go routines to vacate
	done := make(chan any)
	go func() {
		logging.Printf("Waiting for Media routines to vacate")
		s.wg.Wait()
		logging.Printf("Media routines exited")
		close(done)
	}()

	// Either timeout with context or go routines vacate and we close normally
	select {
	case <-ctx.Done():
		logging.Printf("Media stream ctx timeout")
		return ctx.Err()
	case <-done:
		logging.Printf("Media stream closed through done channel")
		if s.Errors != nil {
			close(s.Errors)
		}
		return nil
	}
}
