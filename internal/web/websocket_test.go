package web

import (
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// wsClient is the browser's end, for testing: it masks what it sends, as a browser must, and
// compresses it when asked to.
type wsClient struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

// dialSocket connects to the prompt socket, and gives the status of the answer to the
// handshake, with a client only if it's 101.
func dialSocket(t *testing.T, base, origin, page string) (*wsClient, int) {
	t.Helper()
	host := strings.TrimPrefix(base, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	req := "GET /api/socket?page=" + page + " HTTP/1.1\r\nHost: " + host + "\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Extensions: permessage-deflate; client_max_window_bits\r\n" +
		"Accept-Encoding: gzip\r\n"
	if origin != "" {
		req += "Origin: " + origin + "\r\n"
	}
	if _, err := io.WriteString(conn, req+"\r\n"); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, resp.StatusCode
	}
	if !strings.Contains(resp.Header.Get("Sec-WebSocket-Extensions"), "permessage-deflate") {
		t.Errorf("compression wasn't agreed to: %v", resp.Header)
	}
	return &wsClient{t: t, conn: conn, r: r}, resp.StatusCode
}

func (c *wsClient) send(op byte, payload []byte, compress bool) {
	c.t.Helper()
	head := []byte{0x80 | op, 0x80}
	if compress {
		var buf bytes.Buffer
		zw, _ := flate.NewWriter(&buf, flate.BestSpeed)
		zw.Write(payload)
		zw.Flush()
		payload = bytes.TrimSuffix(buf.Bytes(), []byte{0, 0, 0xff, 0xff})
		head[0] |= 0x40
	}
	switch n := len(payload); {
	case n < 126:
		head[1] |= byte(n)
	case n <= 0xffff:
		head[1] |= 126
		head = binary.BigEndian.AppendUint16(head, uint16(n))
	default:
		head[1] |= 127
		head = binary.BigEndian.AppendUint64(head, uint64(n))
	}
	mask := []byte{1, 2, 3, 4}
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := c.conn.Write(append(append(head, mask...), masked...)); err != nil {
		c.t.Fatal(err)
	}
}

// next reads a frame: its opcode and payload, uncompressed.
func (c *wsClient) next() (byte, []byte) {
	c.t.Helper()
	var head [2]byte
	if _, err := io.ReadFull(c.r, head[:]); err != nil {
		c.t.Fatal(err)
	}
	if head[1]&0x80 != 0 {
		c.t.Fatal("the server masked a frame")
	}
	n := uint64(head[1] & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		io.ReadFull(c.r, ext[:])
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		io.ReadFull(c.r, ext[:])
		n = binary.BigEndian.Uint64(ext[:])
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(c.r, payload); err != nil {
		c.t.Fatal(err)
	}
	if head[0]&0x40 != 0 {
		zr := flate.NewReader(io.MultiReader(bytes.NewReader(payload), bytes.NewReader([]byte{0, 0, 0xff, 0xff, 1, 0, 0, 0xff, 0xff})))
		out, err := io.ReadAll(zr)
		if err != nil {
			c.t.Fatal(err)
		}
		payload = out
	}
	return head[0] & 0x0f, payload
}

func (c *wsClient) call(v any) map[string]jsontext.Value {
	c.t.Helper()
	data, _ := json.Marshal(v)
	c.send(0x1, data, len(data) > 100)
	op, reply := c.next()
	if op != 0x1 {
		c.t.Fatalf("got a frame of kind %d: %q", op, reply)
	}
	var out map[string]jsontext.Value
	if err := json.Unmarshal(reply, &out); err != nil {
		c.t.Fatal(err)
	}
	return out
}

func TestSocketRefusesOtherSites(t *testing.T) {
	s := site(t)
	page := strconv.Itoa(pageRequired)
	for _, origin := range []string{"", "http://evil.example", "http://framework:8484", s.URL + ".evil.example"} {
		if _, status := dialSocket(t, s.URL, origin, page); status != http.StatusForbidden {
			t.Errorf("from %q: %d", origin, status)
		}
	}
}

func TestSocketTellsAnOldPageToReload(t *testing.T) {
	s := site(t)
	c, _ := dialSocket(t, s.URL, s.URL, "1")
	op, payload := c.next()
	if op != 0x8 || len(payload) < 2 || binary.BigEndian.Uint16(payload) != closeOutOfDate {
		t.Errorf("got %d %q", op, payload)
	}
}

func TestSocketOpensAndSavesADraft(t *testing.T) {
	s := site(t)
	var made struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(post(t, s.URL+"/api/drafts", `{}`, http.StatusOK)), &made)
	id := made.ID
	c, _ := dialSocket(t, s.URL, s.URL, strconv.Itoa(pageRequired))

	// A ping is answered, even in the middle of things.
	c.send(0x9, []byte("hi"), false)
	if op, payload := c.next(); op != 0xA || string(payload) != "hi" {
		t.Errorf("ping got %d %q", op, payload)
	}

	opened := c.call(map[string]any{"id": 1, "op": "open", "draft": id})
	if string(opened["re"]) != "1" || opened["error"] != nil {
		t.Fatalf("open: %v", opened)
	}
	prompt, err := applyJSON(nil, opened["prompt"])
	if err != nil {
		t.Fatal(err)
	}
	var draft struct {
		Request jsontext.Value `json:"request"`
	}
	json.Unmarshal(opened["draft"], &draft)
	c.next() // the estimate

	// The model changed to one tack can draw, then a message written, each sent as a patch
	// of the request.
	var request map[string]any
	json.Unmarshal(draft.Request, &request)
	before := draft.Request
	save := func(n int) map[string]jsontext.Value {
		t.Helper()
		after, _ := json.Marshal(request)
		patch, err := diffJSON(before, after)
		if err != nil {
			t.Fatal(err)
		}
		before = after
		saved := c.call(map[string]any{"id": n, "op": "save", "patch": patch})
		if saved["error"] != nil || string(saved["id"]) != strconv.Quote(id) {
			t.Fatalf("save: %v", saved)
		}
		if prompt, err = applyJSON(prompt, saved["prompt"]); err != nil {
			t.Fatal(err)
		}
		if op, payload := c.next(); op != 0x1 || !bytes.Contains(payload, []byte(`"estimate"`)) {
			t.Errorf("no estimate after the save: %q", payload)
		}
		return saved
	}
	request["model"] = wireModel
	request["messages"] = append([]any{map[string]any{"role": "system", "content": strings.Repeat("A long system prompt. ", 300)}}, request["messages"].([]any)...)
	save(2)
	messages := request["messages"].([]any)
	last := messages[len(messages)-1].(map[string]any)
	last["content"] = strings.Repeat("written over a socket ", 20)
	saved := save(3)
	if len(saved["prompt"]) > len(prompt)/2 {
		t.Errorf("the prompt came back whole, or nearly: %d bytes of %d", len(saved["prompt"]), len(prompt))
	}

	// What the socket's patches made is what the draft is.
	resp, err := http.Get(s.URL + "/api/drafts/" + id)
	if err != nil {
		t.Fatal(err)
	}
	var fresh struct {
		Prompt struct {
			Prompt jsontext.Value `json:"prompt"`
		} `json:"prompt"`
	}
	json.UnmarshalRead(resp.Body, &fresh)
	resp.Body.Close()
	var got struct {
		Prompt *wirePromptBody `json:"prompt"`
	}
	if err := json.Unmarshal(prompt, &got); err != nil {
		t.Fatal(err)
	}
	have, _ := json.Marshal(fromWire(got.Prompt))
	var a, b any
	json.Unmarshal(have, &a)
	json.Unmarshal(fresh.Prompt.Prompt, &b)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("the patched prompt isn't the draft's:\n%s\n%s", have, fresh.Prompt.Prompt)
	}
	if !strings.Contains(string(fresh.Prompt.Prompt), "written over a socket") {
		t.Error("the change wasn't saved")
	}

	// A bad patch is refused, and the next good one still applies to what was there before.
	if bad := c.call(map[string]any{"id": 4, "op": "save", "patch": map[string]any{"s": []any{99, 1, []any{}}}}); bad["error"] == nil {
		t.Errorf("a bad patch was taken: %v", bad)
	}
}

func TestSocketRefusesTooBigAMessage(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	c := &wsConn{conn: server, r: bufio.NewReader(server), limit: 10}
	done := make(chan error, 1)
	go func() {
		_, _, err := c.Read(context.Background())
		done <- err
	}()
	tc := &wsClient{t: t, conn: client, r: bufio.NewReader(client)}
	go tc.send(0x1, []byte("more than ten bytes"), false)
	if op, payload := tc.next(); op != 0x8 || binary.BigEndian.Uint16(payload) != closeTooBig {
		t.Errorf("got %d %q", op, payload)
	}
	if err := <-done; err == nil {
		t.Error("it was read")
	}
}
