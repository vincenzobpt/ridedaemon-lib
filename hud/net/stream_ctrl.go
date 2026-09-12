package net

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/charliecharlieO-o/ridedaemon-go/internal/logging"
)

const mediaCtrlHeaderSize = 8

// Command requests
const (
	MediaCtrlInit       uint16 = 16
	MediaCtrlScreenConf uint16 = 96
	MediaCtrlChk        uint16 = 112
	MediaCtrlPing       uint16 = 64
)

// Video codec ids carried in REQ_RV_CONFIG_CAPTURE's wantEncoder and echoed in the reply.
// The values are EasyConn's own: ECTinyPlus.proto declares
// VideoCodecType { NONE=0, JEPG=1, H264=2, MP4=3 }, and net.easyconn.carman's mirror sender
// branches on exactly this field to choose between JPEG stills and an H.264 stream.
const (
	mediaEncoderJpeg uint32 = 1
	mediaEncoderH264 uint32 = 2
)

// Command responses
const (
	MediaCtrlAck       uint16 = 17
	MediaCtrlViewState uint16 = 97
	MediaCtrlRcv       uint16 = 113
	MediaCtrlPong      uint16 = 65
)

type MediaCtrlResponse struct {
	Command uint16
	Size    uint16
	Padding uint32
	Payload []byte
}

type ViewConfig struct {
	State int `json:"state"`
}

type View struct {
	ViewAreaConfig  ViewConfig `json:"viewAreaConfig"`
	SupportFunction int        `json:"supportFunction"`
}

type MediaControl struct {
	port     string
	quit     chan any
	wg       sync.WaitGroup
	listener net.Listener
	tracker  *ConnTracker

	stopOnce sync.Once

	Errors          chan error
	Events          chan MediaCtrlResponse
	OnVideoStart    func()
	SupportFunction int
	// JpegStills answers REQ_RV_CONFIG_CAPTURE with encoder=1 (JPEG) whatever the dash
	// asked for. It is an experiment, not a negotiation: the reply is where the official
	// EasyConn app writes the encoder it will actually use, and one dash family asks for
	// H.264, drains the stream and paints nothing. See buildMediaCaptureAckPayload.
	JpegStills bool
	// OnCaptureNegotiated reports the supportExtendProtocol byte the dash asked for
	// and we echoed back, once the capture-config reply is on the wire.
	OnCaptureNegotiated func(extended bool)
}

func NewMediaControl(port string) *MediaControl {
	return &MediaControl{
		port:    port,
		quit:    make(chan any),
		tracker: NewConnTracker(),
		Errors:  make(chan error, 16),
		Events:  make(chan MediaCtrlResponse, 16),
	}
}

func (s *MediaControl) emitEvent(evt MediaCtrlResponse) {
	select {
	case s.Events <- evt:
	default:
		// channel full, drop
	}
}

func (s *MediaControl) emitError(err error) {
	select {
	case s.Errors <- err:
	default:
		logging.Printf("Dropping error from MediaControl [No one's listening!]: %v", err)
	}
}

// connectionError decides whether one connection's failure ends the session, on the same rule the
// PXC server has used since a dash's abandoned CAR_DATA channel was found tearing down working
// sessions: fatal only when nothing else is being served here.
//
// The bike is KNOWN to open :10922 twice and abandon one of them; whether it does the same here has
// not been proven on a dash, so this is the same rule applied to the same shape of listener rather
// than a fix for a failure already seen on :10921. It can only ever delay a verdict, never invent
// one: a dash that really goes away kills every connection, and whichever one notices last finds
// itself alone and says so.
//
// The fatal text is unchanged, character for character: failures are grouped downstream by the
// text of that line, so the sentence a real death produces has to go on reading the same.
func (s *MediaControl) connectionError(conn net.Conn, errType CtrlErrorType, cause error) *CtrlError {
	if s.tracker.Others(conn) == 0 {
		return &CtrlError{errType, cause, true}
	}
	// The host relays a non-fatal transport error to the rider as a notice, so this one says what
	// happened before it says what the socket complained about.
	return &CtrlError{
		errType,
		fmt.Errorf(
			"one media control channel closed, %d still serving the dash: %v",
			s.tracker.Others(conn), cause,
		),
		false,
	}
}

// isStopping reports our own teardown, so the read that CloseAll is about to interrupt does not
// come back as a fault. Without it every stop produced a fatal "use of closed network connection"
// on this port - the first line of a field teardown (2026-08-26), and the one that made a
// dash-side death look like ours.
func (s *MediaControl) isStopping() bool {
	select {
	case <-s.quit:
		return true
	default:
		return false
	}
}

func (s *MediaControl) decodeHeader(b []byte) (*MediaCtrlResponse, error) {
	if len(b) < mediaCtrlHeaderSize {
		return nil, errors.New("invalid header")
	}
	return &MediaCtrlResponse{
		Command: binary.LittleEndian.Uint16(b[0:2]),
		Size:    binary.LittleEndian.Uint16(b[2:4]),
		Padding: binary.LittleEndian.Uint32(b[4:8]),
	}, nil
}

func (s *MediaControl) acceptLoop() {
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

		logging.Printf("New MediaControl client from %s", conn.RemoteAddr())
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

func (s *MediaControl) writeResponse(res *MediaCtrlResponse, conn net.Conn) error {
	// -- write header
	if err := binary.Write(conn, binary.LittleEndian, res.Command); err != nil {
		return err
	}
	if err := binary.Write(conn, binary.LittleEndian, res.Size); err != nil {
		return err
	}
	if err := binary.Write(conn, binary.LittleEndian, res.Padding); err != nil {
		return err
	}

	// -- write payload
	if len(res.Payload) > 0 {
		if _, err := conn.Write(res.Payload); err != nil {
			return err
		}
	}
	return nil
}

func (s *MediaControl) handleEvent(event *MediaCtrlResponse, conn net.Conn) {
	switch event.Command {
	case MediaCtrlInit:
		s.emitEvent(*event)
		payload := buildMediaCaptureAckPayload(event.Payload, s.JpegStills)
		response := &MediaCtrlResponse{Command: MediaCtrlAck, Size: uint16(len(payload)), Payload: payload}
		if err := s.writeResponse(response, conn); err != nil {
			s.emitError(s.connectionError(conn, CtrlWriteErr, err))
			break
		}
		// Read the framing decision from the payload we built, not from its length:
		// an 8-byte reply is now the normal shape for a dash that asked for no extend
		// protocol, and treating it as "nothing to report" would have silently stopped
		// the transport from being told which framing this session uses.
		if s.OnCaptureNegotiated != nil {
			s.OnCaptureNegotiated(len(payload) >= 9 && payload[8] != 0)
		}
	case MediaCtrlScreenConf:
		s.emitEvent(*event)
		viewState := View{
			ViewAreaConfig:  ViewConfig{State: 0},
			SupportFunction: s.SupportFunction,
		}
		var payload []byte
		if p, err := json.Marshal(viewState); err != nil {
			s.emitError(&CtrlError{CtrlDecodeErr, err, true})
			break
		} else {
			payload = p
		}
		response := &MediaCtrlResponse{Command: MediaCtrlViewState, Size: uint16(len(payload)), Payload: payload}
		if err := s.writeResponse(response, conn); err != nil {
			s.emitError(s.connectionError(conn, CtrlWriteErr, err))
			break
		}
	case MediaCtrlChk:
		if s.OnVideoStart != nil {
			s.OnVideoStart()
		}
		s.emitEvent(*event)
		response := &MediaCtrlResponse{Command: MediaCtrlRcv, Size: 0}
		if err := s.writeResponse(response, conn); err != nil {
			s.emitError(s.connectionError(conn, CtrlWriteErr, err))
			break
		}
	case MediaCtrlPing:
		response := &MediaCtrlResponse{Command: MediaCtrlPong, Size: 0}
		if err := s.writeResponse(response, conn); err != nil {
			s.emitError(s.connectionError(conn, CtrlWriteErr, err))
			break
		}
	default:
		s.emitEvent(*event)
		// Try to send a default command + 1 empty response
		response := &MediaCtrlResponse{Command: event.Command + 1, Size: 0}
		if err := s.writeResponse(response, conn); err != nil {
			s.emitError(s.connectionError(conn, CtrlWriteErr, err))
			break
		}
	}
}

// buildMediaCaptureAckPayload builds RLY_RV_CONFIG_CAPTURE, the reply that tells the dash
// which encoder, geometry and framing the phone will actually use.
//
// forceJpeg answers encoder=1 (VideoCodecType.JEPG in EasyConn's own ECTinyPlus.proto:
// NONE=0, JEPG=1, H264=2, MP4=3) even when the dash asked for H.264. That is deliberate and
// is the whole experiment: the official app dispatches its mirror sender on this field, and
// a dash that asks for H.264 while painting none of the H.264 it pulls is the one case where
// offering it the other format is worth a rider's session. Everything else keeps the echo.
func buildMediaCaptureAckPayload(request []byte, forceJpeg bool) []byte {
	encoder := mediaEncoderH264
	width := uint16(800)
	height := uint16(384)
	extendedProtocol := byte(1)

	if len(request) >= 4 {
		requestedWidth := binary.LittleEndian.Uint16(request[0:2])
		requestedHeight := binary.LittleEndian.Uint16(request[2:4])
		if aligned := requestedWidth &^ 0x0f; aligned >= 16 {
			width = aligned
		}
		if aligned := requestedHeight &^ 0x0f; aligned >= 16 {
			height = aligned
		}
	}
	if len(request) >= 12 {
		if requestedEncoder := binary.LittleEndian.Uint32(request[8:12]); requestedEncoder != 0 {
			encoder = requestedEncoder
		}
	}
	if len(request) >= 30 {
		extendedProtocol = request[29]
	}
	if forceJpeg {
		encoder = mediaEncoderJpeg
	}

	// Protocol.RlyConfigCapture.size() in the CarbitRide APK returns 8 unless
	// supportExtendProtocol is non-zero, and toByteArray() appends the byte only in
	// that case. This built a 9-byte reply either way, so every dashboard that runs
	// without the extend protocol - which is the whole family that reports
	// supportExtendProtocol=0, a QJ 5-inch panel among them - was being handed a
	// trailing zero the official app never sends.
	size := 8
	if extendedProtocol != 0 {
		size = 9
	}
	payload := make([]byte, size)
	binary.LittleEndian.PutUint32(payload[0:4], encoder)
	binary.LittleEndian.PutUint16(payload[4:6], width)
	binary.LittleEndian.PutUint16(payload[6:8], height)
	if size == 9 {
		payload[8] = extendedProtocol
	}
	logging.Printf(
		"Media capture negotiated encoder=%d width=%d height=%d extended=%d",
		encoder,
		width,
		height,
		extendedProtocol,
	)
	return payload
}

// Handling individual TCP Connection/Client
func (s *MediaControl) handleConn(conn net.Conn) {
	s.tracker.Add(conn)
	defer func() {
		s.tracker.Remove(conn)
		_ = conn.Close()
		s.wg.Done()
	}()

	reader := bufio.NewReader(conn)

	// Retire the connection before judging its failure, never after: when the dash goes away every
	// channel fails at once and whichever one is judged last has to find an empty tracker.
	fail := func(errType CtrlErrorType, cause error) {
		if s.isStopping() {
			return
		}
		s.tracker.Remove(conn)
		s.emitError(s.connectionError(conn, errType, cause))
	}

	logging.Printf("Starting StreamCtrl Loop")
	defer logging.Printf("Stopping StreamCtrl Loop")
	for {
		var request *MediaCtrlResponse

		// Read 8 byte header
		headerBytes := make([]byte, mediaCtrlHeaderSize)
		if n, err := io.ReadFull(reader, headerBytes); err != nil {
			fail(CtrlDecodeErr, fmt.Errorf("error reading header: %v (read %d bytes: %x)", err, n, headerBytes[:n]))
			return
		}
		if req, err := s.decodeHeader(headerBytes); err != nil {
			fail(CtrlDecodeErr, fmt.Errorf("error decoding header: %v", err))
			return
		} else {
			request = req
		}

		// Read body if payload is greater than 0
		var payload []byte
		if request.Size > 0 {
			payload = make([]byte, request.Size)
			if _, err := io.ReadFull(reader, payload); err != nil {
				fail(CtrlDecodeErr, fmt.Errorf("[MediaControl] read payload failed from %s: %v", conn.RemoteAddr(), err))
				return
			}
			request.Payload = payload
		}

		// Decide what to do with the request
		s.handleEvent(request, conn)
	}
}

func (s *MediaControl) Start() error {
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

func (s *MediaControl) Stop(ctx context.Context) error {
	logging.Printf("Stopping media control")
	// Signal connection accept loop to stop
	s.stopOnce.Do(func() {
		close(s.quit)
		// Close listener
		if s.listener != nil {
			if err := s.listener.Close(); err != nil {
				logging.Printf("[TCPService] error closing listener: %v", err)
			}
		}
	})

	// Close all tcp connections
	s.tracker.CloseAll()

	// Wait for go routines to vacate
	done := make(chan any)
	go func() {
		logging.Printf("Waiting for Stream Ctrl routines to vacate")
		s.wg.Wait()
		logging.Printf("Stream Ctrl routines vacated")
		close(done)
	}()

	// Either timeout with context or go routines vacate and we close normally
	select {
	case <-ctx.Done():
		logging.Printf("Stream Ctrl ctx timeout")
		return ctx.Err()
	case <-done:
		logging.Printf("Stream Ctrl closed through done channel")
		if s.Errors != nil {
			close(s.Errors)
		}
		if s.Events != nil {
			close(s.Events)
		}
		return nil
	}
}
