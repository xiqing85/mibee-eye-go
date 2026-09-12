// Package rtmp implements a pure-Go RTMP push client.
//
// It connects to a remote RTMP server, performs the RTMP handshake,
// sends command messages (connect, createStream, publish), and pushes
// H.264 video as FLV-encapsulated RTMP data messages.
//
// References:
//   - Adobe RTMP Specification (spec 1.0)
//   - FLV File Format Specification
//   - Various open-source RTMP implementations
package rtmp

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/h264"
)

// Protocol constants.
const (
	rtmpVersion       = 0x03
	rtmpHandshakeSize = 1536

	// Chunk stream IDs
	csIDProtocol   = 2 // protocol control messages (SetChunkSize, etc.)
	csIDCommand    = 3 // command messages (connect, createStream, publish)
	csIDVideo      = 6 // video data stream

	// Message type IDs
	msgTypeSetChunkSize          = 0x01
	msgTypeAbort                 = 0x02
	msgTypeAcknowledgement       = 0x03
	msgTypeWindowAcknowledgement = 0x04
	msgTypeSetPeerBandwidth      = 0x05
	msgTypeAudio                 = 0x08
	msgTypeVideo                 = 0x09
	msgTypeDataAMF               = 0x12
	msgTypeCommandAMF            = 0x14

	// User control message types
	userCtrlStreamBegin = 0x00

	defaultChunkSize    = 128
	preferredChunkSize  = 4096

	defaultPort = 1935
)

// Push sends an H.264 stream to an RTMP server.
type Push struct {
	cfg    Config
	hub    *h264.AUHub

	mu       sync.Mutex
	conn     net.Conn
	stopOnce sync.Once
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	// Cached SPS/PPS from latest keyframe (raw NALU data, no start codes).
	sps []byte
	pps []byte

	// Current chunk size negotiated with server.
	chunkSize uint32

	// PTS tracking (90kHz → ms conversion).
	// basePTS is the PTS of the first AU seen.
	hasBaseTime bool
	baseTime    time.Time
}

// Config holds RTMP push configuration.
type Config struct {
	// RTMP server URL (e.g. rtmp://host:port/app/streamkey).
	URL string `yaml:"url"`

	// Hub is the H.264 access unit hub to subscribe to.
	Hub *h264.AUHub

	// MaxRetries is the maximum number of reconnection attempts (0 = unlimited).
	MaxRetries int
}

// Errors.
var (
	errUnexpectedEOF  = errors.New("rtmp: unexpected EOF")
	errAMFType        = errors.New("rtmp: unexpected AMF type")
	errHandshake      = errors.New("rtmp: handshake failed")
	errConnectRejected = errors.New("rtmp: connect rejected")
	errStreamRejected  = errors.New("rtmp: createStream rejected")
	errPublishRejected = errors.New("rtmp: publish rejected")
)

// New creates a new RTMP push client.
func New(cfg Config) *Push {
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 10 // default
	}
	return &Push{
		cfg:       cfg,
		hub:       cfg.Hub,
		chunkSize: defaultChunkSize,
	}
}

// Start begins the RTMP push loop in a background goroutine.
// Returns immediately. The push loop auto-reconnects on failure.
func (p *Push) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	p.cancel = cancel

	p.wg.Add(1)
	go p.run(ctx)
	return nil
}

// Stop terminates the push loop and closes the connection.
func (p *Push) Stop() error {
	p.stopOnce.Do(func() {
		if p.cancel != nil {
			p.cancel()
		}
		p.closeConn()
		p.wg.Wait()
	})
	return nil
}

// run is the reconnection loop with exponential backoff.
func (p *Push) run(ctx context.Context) {
	defer p.wg.Done()

	backoff := time.Second
	const maxBackoff = 30 * time.Second
	retries := 0

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := p.connectAndPush(ctx)
		if ctx.Err() != nil {
			return
		}

		retries++
		if p.cfg.MaxRetries > 0 && retries >= p.cfg.MaxRetries {
			slog.Error("rtmp: max retries reached, giving up",
				"url", p.cfg.URL, "retries", retries)
			return
		}

		slog.Warn("rtmp: disconnected, reconnecting",
			"url", p.cfg.URL, "error", err,
			"backoff", backoff, "retry", retries)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			backoff = min(backoff*2, maxBackoff)
		}
	}
}

// connectAndPush performs a single RTMP session: handshake → commands → push loop.
func (p *Push) connectAndPush(ctx context.Context) error {
	addr, app, streamKey, err := parseRTMPURL(p.cfg.URL)
	if err != nil {
		return fmt.Errorf("rtmp: parse url: %w", err)
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("rtmp: dial: %w", err)
	}
	p.mu.Lock()
	p.conn = conn
	p.chunkSize = defaultChunkSize
	p.hasBaseTime = false
	p.mu.Unlock()

	// Use a buffered writer for the connection.
	bw := bufio.NewWriter(conn)
	br := bufio.NewReaderSize(conn, 65536)

	slog.Info("rtmp: connected", "url", p.cfg.URL, "addr", addr)

	// Step 1: RTMP Handshake
	if err := p.doHandshake(br, bw); err != nil {
		p.closeConn()
		return fmt.Errorf("rtmp: handshake: %w", err)
	}
	slog.Debug("rtmp: handshake complete")

	// Step 2: Set chunk size for outgoing messages
	if err := p.writeSetChunkSize(bw, preferredChunkSize); err != nil {
		p.closeConn()
		return fmt.Errorf("rtmp: set chunk size: %w", err)
	}

	// Start read goroutine (processes incoming chunks: ping, ack, etc.)
	readCtx, readCancel := context.WithCancel(ctx)
	readDone := make(chan error, 1)
	go func() {
		readDone <- p.readLoop(readCtx)
	}()

	// Step 3: Send connect command and wait for response
	if err := p.writeConnect(bw, app, addr); err != nil {
		readCancel()
		p.closeConn()
		return fmt.Errorf("rtmp: connect: %w", err)
	}
	if err := bw.Flush(); err != nil {
		readCancel()
		p.closeConn()
		return err
	}
	slog.Debug("rtmp: connect sent", "app", app)

	// Wait for connect result (read loop will signal)
	// We need to read responses synchronously. Let's use a helper.
	// Read _result for connect (transaction ID 1)
	resp, err := p.waitCommand(br, "_result", 1.0)
	if err != nil {
		readCancel()
		p.closeConn()
		return fmt.Errorf("rtmp: connect response: %w", err)
	}
	_ = resp // properties not needed
	slog.Info("rtmp: connect successful")

	// Step 4: Send createStream
	if err := p.writeCreateStream(bw); err != nil {
		readCancel()
		p.closeConn()
		return fmt.Errorf("rtmp: createStream: %w", err)
	}
	if err := bw.Flush(); err != nil {
		readCancel()
		p.closeConn()
		return err
	}

	// Read _result for createStream (transaction ID 2)
	resp, err = p.waitCommand(br, "_result", 2.0)
	if err != nil {
		readCancel()
		p.closeConn()
		return fmt.Errorf("rtmp: createStream response: %w", err)
	}

	// Extract stream ID from response
	streamID, err := p.parseStreamID(resp)
	if err != nil {
		readCancel()
		p.closeConn()
		return fmt.Errorf("rtmp: parse stream id: %w", err)
	}
	slog.Info("rtmp: stream created", "stream_id", streamID)

	// Step 5: Send publish
	if err := p.writePublish(bw, streamKey); err != nil {
		readCancel()
		p.closeConn()
		return fmt.Errorf("rtmp: publish: %w", err)
	}
	if err := bw.Flush(); err != nil {
		readCancel()
		p.closeConn()
		return err
	}

	// Wait for onStatus (publish success)
	_, err = p.waitCommand(br, "onStatus", 0.0)
	if err != nil {
		readCancel()
		p.closeConn()
		return fmt.Errorf("rtmp: publish response: %w", err)
	}
	slog.Info("rtmp: publish started", "stream_key", streamKey)

	// Step 6: Subscribe to AUHub and push video frames
	p.pushLoop(ctx, br, bw, streamID)

	readCancel()
	<-readDone
	p.closeConn()
	return ctx.Err()
}

// pushLoop reads access units from the hub and sends them as FLV video tags.
func (p *Push) pushLoop(ctx context.Context, br *bufio.Reader, bw *bufio.Writer, streamID uint32) {
	sub := p.hub.Subscribe(ctx)
	defer func() {
		// Flush any remaining data
		_ = bw.Flush()
	}()

	
	seqHeaderSent := false

	slog.Info("rtmp: push loop started")

	for {
		select {
		case <-ctx.Done():
			return
		case au, ok := <-sub.Channel:
			if !ok {
				return
			}
			p.processAU(bw, au, &seqHeaderSent, streamID)
		}
	}
}

// processAU converts an access unit to FLV and sends it.
func (p *Push) processAU(bw *bufio.Writer, au h264.AccessUnit, seqHeaderSent *bool, streamID uint32) {
	// Extract SPS/PPS from keyframes
	for _, nalu := range au.NALUs {
		if nalu.IsSPS {
			p.mu.Lock()
			p.sps = append([]byte(nil), nalu.Data...)
			p.mu.Unlock()
		}
		if nalu.IsPPS {
			p.mu.Lock()
			p.pps = append([]byte(nil), nalu.Data...)
			p.mu.Unlock()
		}
	}

	// Skip non-IDR until we have SPS/PPS for sequence header
	if !au.KeyFrame {
		// For non-keyframes, skip if no SPS/PPS yet and seq header not sent
		if !*seqHeaderSent {
			return
		}
	}

	// Calculate timestamp in milliseconds from time.Time
	p.mu.Lock()
	if !p.hasBaseTime {
		p.baseTime = au.Timestamp
		p.hasBaseTime = true
	}
	tsMs := uint32(au.Timestamp.Sub(p.baseTime).Milliseconds())
	p.mu.Unlock()

	p.mu.Lock()
	sps := p.sps
	pps := p.pps
	p.mu.Unlock()
	// Send AVC sequence header on keyframes (before the NALU data)
	if au.KeyFrame && len(sps) > 0 && len(pps) > 0 {
		seqHeader, err := makeAVCSequenceHeader(sps, pps)
		if err == nil {
			// FLV tag body for sequence header
			shBody := make([]byte, 5+len(seqHeader))
			shBody[0] = frameTypeKeyFrame | codecIDAVC
			shBody[1] = avcSeqHeader
			shBody[2] = 0 // composition time
			shBody[3] = 0
			shBody[4] = 0
			copy(shBody[5:], seqHeader)

			tag := makeFLVVideoTag(tsMs, shBody)
			if err := p.writeDataMessage(bw, msgTypeVideo, streamID, tsMs, tag[11:]); err != nil {
				slog.Warn("rtmp: write seq header error", "error", err)
				return
			}
		}
		*seqHeaderSent = true
	}

	// Build NALU body (AVCC format: length-prefixed NALUs)
	rawNalus := make([][]byte, 0, len(au.NALUs))
	for _, nalu := range au.NALUs {
		rawNalus = append(rawNalus, nalu.Data)
	}

	var frameType byte = frameTypeInterFrame
	if au.KeyFrame {
		frameType = frameTypeKeyFrame
	}

	naluBody := makeAVCNALU(frameType, rawNalus)
	tag := makeFLVVideoTag(tsMs, naluBody)

	if err := p.writeDataMessage(bw, msgTypeVideo, streamID, tsMs, tag[11:]); err != nil {
		slog.Warn("rtmp: write video data error", "error", err)
	}
}

// waitCommand reads RTMP chunks until it finds a command response matching
// the expected command name and transaction ID.
func (p *Push) waitCommand(br *bufio.Reader, expectCmd string, expectTxn float64) ([]byte, error) {
	for {
		msg, err := p.readMessage(br)
		if err != nil {
			return nil, err
		}
		if msg.msgType == msgTypeCommandAMF {
			cmd, txn, rest, err := parseCommand(msg.data)
			if err != nil {
				continue
			}
			if cmd == expectCmd && txn == expectTxn {
				return rest, nil
			}
		}
	}
}

// parseCommand extracts command name and transaction ID from an AMF0 command message.
func parseCommand(data []byte) (cmd string, txn float64, rest []byte, err error) {
	r := newAMFReader(data)
	cmd, err = r.readString()
	if err != nil {
		return "", 0, nil, err
	}
	txn, err = r.readNumber()
	if err != nil {
		return "", 0, nil, err
	}
	if r.available() > 0 {
		rest = r.data[r.pos:]
	}
	return cmd, txn, rest, nil
}

// parseStreamID extracts the numeric stream ID from a createStream _result.
// The format is: command "string", txn "number", null, streamID "number".
func (p *Push) parseStreamID(data []byte) (uint32, error) {
	r := newAMFReader(data)
	// Skip first null or object (properties)
	m, err := r.readMarker()
	if err != nil {
		return 0, err
	}
	switch m {
	case amfNull:
		// OK
	case amfObject:
		// Skip the object
		for {
			if r.available() < 3 {
				return 0, errUnexpectedEOF
			}
			if r.data[r.pos] == 0 && r.data[r.pos+1] == 0 && r.data[r.pos+2] == 0x09 {
				r.pos += 3
				break
			}
			if err := r.skipUTF8(); err != nil {
				return 0, err
			}
			if err := r.skipValue(); err != nil {
				return 0, err
			}
		}
	default:
		return 0, errAMFType
	}

	// Next should be a number (stream ID)
	if r.available() < 1 {
		return 0, errUnexpectedEOF
	}
	m, err = r.readMarker()
	if err != nil {
		return 0, err
	}
	if m != amfNumber {
		return 0, errAMFType
	}
	if r.pos+8 > len(r.data) {
		return 0, errUnexpectedEOF
	}
	val := math.Float64frombits(binary.BigEndian.Uint64(r.data[r.pos:]))
	return uint32(val), nil
}

// ---------------------------------------------------------------------------
// RTMP Handshake
// ---------------------------------------------------------------------------

func (p *Push) doHandshake(br *bufio.Reader, bw *bufio.Writer) error {
	// C0: 1 byte version
	if err := bw.WriteByte(rtmpVersion); err != nil {
		return err
	}

	// C1: 1536 bytes
	c1 := make([]byte, rtmpHandshakeSize)
	// First 4 bytes: client timestamp (current time ms)
	ts := uint32(time.Now().UnixMilli())
	binary.BigEndian.PutUint32(c1[0:4], ts)
	// Next 4 bytes: zero (RTMP spec: must be 0)
	// Remaining 1528 bytes: random data
	if _, err := rand.Read(c1[8:]); err != nil {
		return fmt.Errorf("random: %w", err)
	}
	if _, err := bw.Write(c1); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}

	// Read S0: 1 byte (must be 0x03)
	s0, err := br.ReadByte()
	if err != nil {
		return fmt.Errorf("s0: %w", err)
	}
	if s0 != rtmpVersion {
		return fmt.Errorf("%w: server version 0x%02x", errHandshake, s0)
	}

	// Read S1: 1536 bytes
	s1 := make([]byte, rtmpHandshakeSize)
	if _, err := io.ReadFull(br, s1); err != nil {
		return fmt.Errorf("s1: %w", err)
	}

	// C2: 1536 bytes — echo of S1
	// First 4 bytes: server timestamp (from S1[0:4])
	// Next 4 bytes: client timestamp (from C1[0:4])
	// Remaining 1528 bytes: S1 random data (S1[8:])
	c2 := make([]byte, rtmpHandshakeSize)
	copy(c2[0:4], s1[0:4])    // echo server timestamp
	copy(c2[4:8], c1[0:4])    // echo client timestamp
	copy(c2[8:], s1[8:])      // echo server random data
	if _, err := bw.Write(c2); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}

	// Read S2: 1536 bytes — echo of C1 (we can discard)
	s2 := make([]byte, rtmpHandshakeSize)
	if _, err := io.ReadFull(br, s2); err != nil {
		return fmt.Errorf("s2: %w", err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// RTMP Chunk Writing
// ---------------------------------------------------------------------------

type rtmpMessage struct {
	csID        int
	msgType     byte
	msgStreamID uint32
	timestamp   uint32
	data        []byte
}

// writeSetChunkSize sends a SetChunkSize protocol message (cs_id=2, msg_type=0x01).
func (p *Push) writeSetChunkSize(bw *bufio.Writer, size uint32) error {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, size)
	return p.writeRawMessage(bw, csIDProtocol, msgTypeSetChunkSize, 0, 0, payload)
}

// writeConnect sends the "connect" command over the control stream.
func (p *Push) writeConnect(bw *bufio.Writer, app string, addr string) error {
	// Build AMF0 command: "connect", transactionId 1, object {app, flashVer, tcUrl, ...}
	w := newAMFWriter()
	w.writeString("connect")
	w.writeNumber(1.0) // transaction ID
	w.writeObjectStart()
	w.writeObjectEntry("app", app)
	w.writeObjectEntry("flashVer", "WIN.0,0,0,0")
	// tcUrl = rtmp://host/app
	parts := strings.SplitN(addr, ":", 2)
	host := parts[0]
	tcURL := fmt.Sprintf("rtmp://%s/%s", host, app)
	w.writeObjectEntry("tcUrl", tcURL)
	w.writeObjectEntry("fpad", false)
	w.writeObjectEntry("capabilities", float64(15))
	w.writeObjectEntry("audioCodecs", float64(0))
	w.writeObjectEntry("videoCodecs", float64(0x0080)) // H264Codec
	w.writeObjectEntry("videoFunction", float64(1))
	w.writeObjectEnd()
	// Optional additional object (empty)
	w.writeObjectStart()
	w.writeObjectEnd()

	return p.writeRawMessage(bw, csIDCommand, msgTypeCommandAMF, 0, 0, w.bytes())
}

// writeCreateStream sends the "createStream" command.
func (p *Push) writeCreateStream(bw *bufio.Writer) error {
	w := newAMFWriter()
	w.writeString("createStream")
	w.writeNumber(2.0) // transaction ID
	w.writeNull()      // command object (optional)
	return p.writeRawMessage(bw, csIDCommand, msgTypeCommandAMF, 0, 0, w.bytes())
}

// writePublish sends the "publish" command on the created stream.
func (p *Push) writePublish(bw *bufio.Writer, streamKey string) error {
	w := newAMFWriter()
	w.writeString("publish")
	w.writeNumber(0.0) // transaction ID
	w.writeNull()      // command object (optional)
	w.writeString(streamKey)
	w.writeString("live") // publishing type: "live", "record", "append"
	return p.writeRawMessage(bw, csIDCommand, msgTypeCommandAMF, 0, 0, w.bytes())
}

// writeDataMessage sends an RTMP data message (audio/video) over a video stream.
// It uses a type 0 chunk header for the message.
func (p *Push) writeDataMessage(bw *bufio.Writer, msgType byte, msgStreamID uint32, timestamp uint32, data []byte) error {
	return p.writeRawMessage(bw, csIDVideo, msgType, msgStreamID, timestamp, data)
}

// writeRawMessage writes a complete RTMP message, chunking as needed.
// The message is split into chunks of p.chunkSize bytes.
func (p *Push) writeRawMessage(bw *bufio.Writer, csID int, msgType byte, msgStreamID uint32, timestamp uint32, data []byte) error {
	p.mu.Lock()
	chunkSize := int(p.chunkSize)
	p.mu.Unlock()

	totalLen := len(data)
	remaining := totalLen

	for i := 0; remaining > 0; i++ {
		chunkDataLen := chunkSize
		if remaining < chunkSize {
			chunkDataLen = remaining
		}

		offset := totalLen - remaining

		if i == 0 {
			// First chunk: type 0 (full header)
			hdr := makeChunkHeader(csID, 0, timestamp, totalLen, msgType, msgStreamID)
			if _, err := bw.Write(hdr); err != nil {
				return err
			}
		} else {
			// Subsequent chunks: type 3 (no header)
			hdr := makeChunkHeader(csID, 3, 0, 0, 0, 0)
			if _, err := bw.Write(hdr); err != nil {
				return err
			}
		}

		if _, err := bw.Write(data[offset : offset+chunkDataLen]); err != nil {
			return err
		}

		remaining -= chunkDataLen
	}

	return nil
}

// makeChunkHeader builds the basic header and (optionally) message header bytes for a chunk.
func makeChunkHeader(csID int, fmt byte, timestamp uint32, msgLen int, msgType byte, msgStreamID uint32) []byte {
	// Determine basic header size.
	var basicHdr []byte
	if csID < 64 {
		basicHdr = []byte{fmt<<6 | byte(csID)}
	} else if csID < 320 {
		// 2-byte header: first byte = (fmt<<6) | 0, second byte = csID - 64
		basicHdr = []byte{fmt << 6, byte(csID - 64)}
	} else {
		// 3-byte header
		basicHdr = []byte{fmt<<6 | 1, byte(csID - 64), byte((csID - 64) >> 8)}
	}

	// Extended timestamp: if timestamp >= 0xFFFFFF, store 0xFFFFFF and
	// include extended timestamp after message header.
	extTS := uint32(0)
	tsField := uint32(timestamp)
	if tsField >= 0xFFFFFF {
		extTS = tsField
		tsField = 0xFFFFFF
	}

	if fmt == 3 {
		// Type 3: no message header, only basic header
		return basicHdr
	}

	var msgHdr []byte
	switch fmt {
	case 0:
		// 11 bytes: timestamp (3), message length (3), msg type (1), msg stream ID (4 LE)
		msgHdr = make([]byte, 11)
		msgHdr[0] = byte(tsField >> 16)
		msgHdr[1] = byte(tsField >> 8)
		msgHdr[2] = byte(tsField)
		msgHdr[3] = byte(msgLen >> 16)
		msgHdr[4] = byte(msgLen >> 8)
		msgHdr[5] = byte(msgLen)
		msgHdr[6] = msgType
		binary.LittleEndian.PutUint32(msgHdr[7:11], msgStreamID)
	case 1:
		// 7 bytes: timestamp delta (3), message length (3), msg type (1)
		msgHdr = make([]byte, 7)
		msgHdr[0] = byte(tsField >> 16)
		msgHdr[1] = byte(tsField >> 8)
		msgHdr[2] = byte(tsField)
		msgHdr[3] = byte(msgLen >> 16)
		msgHdr[4] = byte(msgLen >> 8)
		msgHdr[5] = byte(msgLen)
		msgHdr[6] = msgType
	case 2:
		// 3 bytes: timestamp delta
		msgHdr = make([]byte, 3)
		msgHdr[0] = byte(tsField >> 16)
		msgHdr[1] = byte(tsField >> 8)
		msgHdr[2] = byte(tsField)
	}

	// Append basic header + message header
	result := make([]byte, 0, len(basicHdr)+len(msgHdr)+4)
	result = append(result, basicHdr...)
	result = append(result, msgHdr...)

	// Extended timestamp (4 bytes, big-endian)
	if extTS > 0 {
		var ext [4]byte
		binary.BigEndian.PutUint32(ext[:], extTS)
		result = append(result, ext[:]...)
	}

	return result
}

// ---------------------------------------------------------------------------
// RTMP Chunk Reading (minimal for responses)
// ---------------------------------------------------------------------------

type readMessage struct {
	csID        int
	msgType     byte
	msgStreamID uint32
	timestamp   uint32
	data        []byte
}

// readChunkState tracks reassembly state per chunk stream.
type readChunkState struct {
	msgType     byte
	msgStreamID uint32
	timestamp   uint32
	msgLen      int
	data        []byte
	received    int
}

// readMessage reads one complete RTMP message from the stream.
func (p *Push) readMessage(br *bufio.Reader) (*readMessage, error) {
	states := make(map[int]*readChunkState)

	for {
		// Read basic header
		b, err := br.ReadByte()
		if err != nil {
			return nil, err
		}
		fmt := (b >> 6) & 0x03
		csID := int(b & 0x3F)
		if csID == 0 {
			// 2-byte basic header
			b2, err := br.ReadByte()
			if err != nil {
				return nil, err
			}
			csID = 64 + int(b2)
		} else if csID == 1 {
			// 3-byte basic header
			b2, err := br.ReadByte()
			if err != nil {
				return nil, err
			}
			b3, err := br.ReadByte()
			if err != nil {
				return nil, err
			}
			csID = 64 + int(b2) + int(b3)*256
		}

		// Get or create state for this chunk stream.
		state, ok := states[csID]
		if !ok {
			state = &readChunkState{}
			states[csID] = state
		}

		// Parse message header based on fmt
		switch fmt {
		case 0: // 11-byte header
			hdr := make([]byte, 11)
			if _, err := io.ReadFull(br, hdr); err != nil {
				return nil, err
			}
			state.timestamp = uint32(hdr[0])<<16 | uint32(hdr[1])<<8 | uint32(hdr[2])
			state.msgLen = int(hdr[3])<<16 | int(hdr[4])<<8 | int(hdr[5])
			state.msgType = hdr[6]
			state.msgStreamID = binary.LittleEndian.Uint32(hdr[7:11])

			// Extended timestamp
			if state.timestamp == 0xFFFFFF {
				var ext [4]byte
				if _, err := io.ReadFull(br, ext[:]); err != nil {
					return nil, err
				}
				state.timestamp = binary.BigEndian.Uint32(ext[:])
			}

			state.data = make([]byte, state.msgLen)
			state.received = 0

		case 1: // 7-byte header (timestamp delta, msg length, msg type)
			hdr := make([]byte, 7)
			if _, err := io.ReadFull(br, hdr); err != nil {
				return nil, err
			}
			tsDelta := uint32(hdr[0])<<16 | uint32(hdr[1])<<8 | uint32(hdr[2])
			state.msgLen = int(hdr[3])<<16 | int(hdr[4])<<8 | int(hdr[5])
			state.msgType = hdr[6]

			if tsDelta == 0xFFFFFF {
				var ext [4]byte
				if _, err := io.ReadFull(br, ext[:]); err != nil {
					return nil, err
				}
				tsDelta = binary.BigEndian.Uint32(ext[:])
			}
			state.timestamp += tsDelta

			state.data = make([]byte, state.msgLen)
			state.received = 0

		case 2: // 3-byte header (timestamp delta only)
			hdr := make([]byte, 3)
			if _, err := io.ReadFull(br, hdr); err != nil {
				return nil, err
			}
			tsDelta := uint32(hdr[0])<<16 | uint32(hdr[1])<<8 | uint32(hdr[2])

			if tsDelta == 0xFFFFFF {
				var ext [4]byte
				if _, err := io.ReadFull(br, ext[:]); err != nil {
					return nil, err
				}
				tsDelta = binary.BigEndian.Uint32(ext[:])
			}
			state.timestamp += tsDelta

		case 3: // 0-byte header
			// No additional header; use existing state
		}

		// Read chunk data
		var chunkSize int
		p.mu.Lock()
		chunkSize = int(p.chunkSize)
		p.mu.Unlock()

		remaining := state.msgLen - state.received
		toRead := chunkSize
		if remaining < chunkSize {
			toRead = remaining
		}

		dest := state.data[state.received : state.received+toRead]
		if _, err := io.ReadFull(br, dest); err != nil {
			return nil, err
		}
		state.received += toRead

		// Check if message is complete
		if state.received >= state.msgLen {
			msg := &readMessage{
				csID:        csID,
				msgType:     state.msgType,
				msgStreamID: state.msgStreamID,
				timestamp:   state.timestamp,
				data:        state.data,
			}
			// Reset state (keep for continuation)
			delete(states, csID)
			return msg, nil
		}
	}
}

// readLoop processes incoming messages that aren't command responses.
// This handles protocol messages (SetChunkSize, Ping, etc.).
func (p *Push) readLoop(ctx context.Context) error {
	// This is a placeholder — we read commands synchronously in waitCommand,
	// so the read loop isn't strictly needed for the initial setup.
	// After publishing, the server may send ping/pong and status messages.
	// We handle them here.
	return nil
}

// closeConn safely closes the underlying TCP connection.
func (p *Push) closeConn() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn != nil {
		p.conn.Close()
		p.conn = nil
	}
}

// parseRTMPURL parses an RTMP URL into host:port, app name, and stream key.
// Format: rtmp://host:port/app/streamkey
func parseRTMPURL(rawURL string) (addr, app, streamKey string, err error) {
	// RTMP URLs are not fully RFC-compliant, so brute-force the parsing.
	// Strip "rtmp://" prefix.
	rest := rawURL
	for _, pfx := range []string{"rtmp://", "rtmps://"} {
		if strings.HasPrefix(rest, pfx) {
			rest = rest[len(pfx):]
			break
		}
	}

	// Split host:port from path
	slashIdx := strings.IndexByte(rest, '/')
	var hostPort string
	if slashIdx >= 0 {
		hostPort = rest[:slashIdx]
		rest = rest[slashIdx+1:] // path after host
	} else {
		hostPort = rest
		rest = ""
	}

	// Parse host:port
	host, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		// No port specified
		host = hostPort
		portStr = fmt.Sprintf("%d", defaultPort)
	}

	addr = net.JoinHostPort(host, portStr)

	// Parse path: first segment is app, second (optional) is streamKey
	pathParts := strings.SplitN(rest, "/", 2)
	if len(pathParts) > 0 {
		app = pathParts[0]
	}
	if len(pathParts) > 1 {
		streamKey = pathParts[1]
	}

	if app == "" {
		app = "live"
	}
	if streamKey == "" {
		streamKey = "stream"
	}

	return addr, app, streamKey, nil
}

