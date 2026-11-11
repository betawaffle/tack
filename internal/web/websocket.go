package web

import (
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// A websocket's server end (RFC 6455), with permessage-deflate (RFC 7692) so big messages go
// compressed, as HTTP responses do. It's only what tack needs. The methods are shaped like
// github.com/coder/websocket's Conn, so that package could take its place by changing this
// file and nothing that uses it.

type messageType int

const (
	messageText   messageType = 1
	messageBinary messageType = 2
)

// socket is what's used of a websocket.
type socket interface {
	Read(ctx context.Context) (messageType, []byte, error)
	Write(ctx context.Context, typ messageType, data []byte) error
	Close(code int, reason string) error
}

const (
	closeNormal       = 1000
	closeGoingAway    = 1001
	closeProtocol     = 1002
	closeBadData      = 1007
	closeTooBig       = 1009
	closeOutOfDate    = 4000 // the page is older than the server; it has to be reloaded
	wsPingEvery       = 30 * time.Second
	wsReadIdle        = 75 * time.Second // more than two pings without an answer
	wsWriteTimeout    = 30 * time.Second
	wsCompressAbove   = 512
	wsAcceptGUID      = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	wsMaxControlBytes = 125
)

var errSocketClosed = errors.New("websocket closed")

type wsConn struct {
	conn    net.Conn
	r       *bufio.Reader
	deflate bool
	limit   int64

	wmu    sync.Mutex
	closed bool // a close frame has been sent; guarded by wmu
}

// hasToken says whether a header that's a comma-separated list has a token, whatever its
// case, as HTTP says these are compared.
func hasToken(h http.Header, name, token string) bool {
	for _, value := range h.Values(name) {
		for item := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(item), token) {
				return true
			}
		}
	}
	return false
}

// wantsDeflate says whether the browser offers permessage-deflate in a form tack can use: any,
// so long as it doesn't limit the server's window, which compress/flate can't.
func wantsDeflate(h http.Header) bool {
	for _, value := range h.Values("Sec-WebSocket-Extensions") {
		for offer := range strings.SplitSeq(value, ",") {
			params := strings.Split(offer, ";")
			if strings.TrimSpace(params[0]) != "permessage-deflate" {
				continue
			}
			ok := true
			for _, p := range params[1:] {
				name, _, _ := strings.Cut(strings.TrimSpace(p), "=")
				if name == "server_max_window_bits" {
					ok = false
				}
			}
			if ok {
				return true
			}
		}
	}
	return false
}

// acceptSocket turns a request into a websocket. It has to come from a page of this site:
// the browser always says where a websocket's page is from, and Go's cross-origin protection
// doesn't look at websockets, so it's checked here. The host was checked already (knownHost).
// Messages over limit bytes, once uncompressed, end the connection.
func acceptSocket(w http.ResponseWriter, r *http.Request, limit int64) (*wsConn, error) {
	fail := func(status int, text string) (*wsConn, error) {
		http.Error(w, text, status)
		return nil, errors.New(text)
	}
	if r.Method != http.MethodGet || !hasToken(r.Header, "Connection", "upgrade") || !hasToken(r.Header, "Upgrade", "websocket") {
		return fail(http.StatusBadRequest, "not a websocket request")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		return fail(http.StatusUpgradeRequired, "unsupported websocket version")
	}
	origin := r.Header.Get("Origin")
	if origin == "" || (origin != "http://"+r.Host && origin != "https://"+r.Host) {
		return fail(http.StatusForbidden, "not from a page of this site")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if raw, err := base64.StdEncoding.DecodeString(key); err != nil || len(raw) != 16 {
		return fail(http.StatusBadRequest, "bad websocket key")
	}
	deflate := wantsDeflate(r.Header)
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return fail(http.StatusInternalServerError, "can't take over the connection")
	}
	sum := sha1.Sum([]byte(key + wsAcceptGUID))
	var head strings.Builder
	head.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n")
	head.WriteString("Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n")
	if deflate {
		// Each message is compressed on its own, so neither end keeps a window between them.
		head.WriteString("Sec-WebSocket-Extensions: permessage-deflate; server_no_context_takeover; client_no_context_takeover\r\n")
	}
	head.WriteString("\r\n")
	conn.SetDeadline(time.Time{})
	conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	if _, err := conn.Write([]byte(head.String())); err != nil {
		conn.Close()
		return nil, err
	}
	return &wsConn{conn: conn, r: rw.Reader, deflate: deflate, limit: limit}, nil
}

// Read reads the next whole message, answering pings and a close on the way.
func (c *wsConn) Read(ctx context.Context) (messageType, []byte, error) {
	stop := context.AfterFunc(ctx, func() { c.conn.SetReadDeadline(time.Unix(1, 0)) })
	defer stop()
	var (
		typ        messageType
		compressed bool
		data       []byte
		started    bool
	)
	for {
		c.conn.SetReadDeadline(time.Now().Add(wsReadIdle))
		var head [2]byte
		if _, err := io.ReadFull(c.r, head[:]); err != nil {
			return 0, nil, err
		}
		fin, rsv1, op := head[0]&0x80 != 0, head[0]&0x40 != 0, head[0]&0x0f
		if head[0]&0x30 != 0 || head[1]&0x80 == 0 {
			return 0, nil, c.fail(closeProtocol, "bad frame")
		}
		n := int64(head[1] & 0x7f)
		switch n {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(c.r, ext[:]); err != nil {
				return 0, nil, err
			}
			n = int64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(c.r, ext[:]); err != nil {
				return 0, nil, err
			}
			n = int64(binary.BigEndian.Uint64(ext[:]) & (1<<63 - 1))
		}
		control := op&0x8 != 0
		if control && (n > wsMaxControlBytes || !fin || rsv1) {
			return 0, nil, c.fail(closeProtocol, "bad control frame")
		}
		if !control && int64(len(data))+n > c.limit {
			return 0, nil, c.fail(closeTooBig, "message too big")
		}
		var mask [4]byte
		if _, err := io.ReadFull(c.r, mask[:]); err != nil {
			return 0, nil, err
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(c.r, payload); err != nil {
			return 0, nil, err
		}
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
		switch op {
		case 0x8: // close: answer it, and that's the end
			code := closeNormal
			if len(payload) >= 2 {
				code = int(binary.BigEndian.Uint16(payload))
			}
			c.Close(code, "")
			return 0, nil, errSocketClosed
		case 0x9: // ping
			if err := c.writeFrame(0xA, false, payload); err != nil {
				return 0, nil, err
			}
			continue
		case 0xA: // pong
			continue
		case 0x1, 0x2:
			if started {
				return 0, nil, c.fail(closeProtocol, "a new message in the middle of one")
			}
			if rsv1 && !c.deflate {
				return 0, nil, c.fail(closeProtocol, "compressed without agreeing to")
			}
			started, typ, compressed = true, messageType(op), rsv1
		case 0x0:
			if !started || rsv1 {
				return 0, nil, c.fail(closeProtocol, "bad continuation")
			}
		default:
			return 0, nil, c.fail(closeProtocol, "unknown frame")
		}
		data = append(data, payload...)
		if !fin {
			continue
		}
		if compressed {
			// The end of a message's compressed data is left off (RFC 7692 7.2.2); this puts
			// it back, and a final empty block so reading stops.
			zr := flate.NewReader(io.MultiReader(bytes.NewReader(data), bytes.NewReader([]byte{0, 0, 0xff, 0xff, 1, 0, 0, 0xff, 0xff})))
			out, err := io.ReadAll(io.LimitReader(zr, c.limit+1))
			zr.Close()
			if err != nil {
				return 0, nil, c.fail(closeBadData, "bad compressed data")
			}
			if int64(len(out)) > c.limit {
				return 0, nil, c.fail(closeTooBig, "message too big")
			}
			data = out
		}
		if typ == messageText && !utf8.Valid(data) {
			return 0, nil, c.fail(closeBadData, "text that isn't UTF-8")
		}
		return typ, data, nil
	}
}

var flateWriters = sync.Pool{New: func() any {
	w, _ := flate.NewWriter(nil, flate.DefaultCompression)
	return w
}}

// Write sends a message, compressed if it's big enough to be worth it.
func (c *wsConn) Write(ctx context.Context, typ messageType, data []byte) error {
	compressed := false
	if c.deflate && len(data) > wsCompressAbove {
		var buf bytes.Buffer
		zw := flateWriters.Get().(*flate.Writer)
		zw.Reset(&buf)
		zw.Write(data)
		zw.Flush()
		flateWriters.Put(zw)
		out := buf.Bytes()
		if bytes.HasSuffix(out, []byte{0, 0, 0xff, 0xff}) {
			data, compressed = out[:len(out)-4], true
		}
	}
	return c.writeFrame(byte(typ), compressed, data)
}

func (c *wsConn) writeFrame(op byte, compressed bool, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return errSocketClosed
	}
	return c.writeLocked(op, compressed, payload)
}

func (c *wsConn) writeLocked(op byte, compressed bool, payload []byte) error {
	head := []byte{0x80 | op, 0}
	if compressed {
		head[0] |= 0x40
	}
	switch n := len(payload); {
	case n < 126:
		head[1] = byte(n)
	case n <= 0xffff:
		head[1] = 126
		head = binary.BigEndian.AppendUint16(head, uint16(n))
	default:
		head[1] = 127
		head = binary.BigEndian.AppendUint64(head, uint64(n))
	}
	c.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	bufs := net.Buffers{head, payload}
	_, err := bufs.WriteTo(c.conn)
	return err
}

// Close says why it's closing and closes the connection.
func (c *wsConn) Close(code int, reason string) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if !c.closed {
		c.closed = true
		if len(reason) > wsMaxControlBytes-2 {
			reason = reason[:wsMaxControlBytes-2]
		}
		c.writeLocked(0x8, false, append(binary.BigEndian.AppendUint16(nil, uint16(code)), reason...))
	}
	return c.conn.Close()
}

// fail closes the connection for something the other end did wrong, and says what.
func (c *wsConn) fail(code int, reason string) error {
	c.Close(code, reason)
	return fmt.Errorf("websocket: %s", reason)
}

// keepAlive pings every so often until ctx is done, so a connection that's quietly gone is
// noticed, and one through something that drops idle connections stays up.
func (c *wsConn) keepAlive(ctx context.Context) {
	t := time.NewTicker(wsPingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if c.writeFrame(0x9, false, nil) != nil {
				return
			}
		}
	}
}
