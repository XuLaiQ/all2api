package cdp

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

type WebSocket struct {
	conn   net.Conn
	reader *bufio.Reader
	mu     sync.Mutex
	nextID int64
}

func Dial(ctx context.Context, rawURL string) (*WebSocket, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "ws" || parsed.Host == "" {
		return nil, errors.New("CDP websocket URL is invalid")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", parsed.Host)
	if err != nil {
		return nil, errors.New("CDP browser connection failed")
	}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		_ = conn.Close()
		return nil, errors.New("CDP websocket handshake failed")
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	if parsed.RawQuery != "" {
		path += "?" + parsed.RawQuery
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	request := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", path, parsed.Host, key)
	if _, err := io.WriteString(conn, request); err != nil {
		_ = conn.Close()
		return nil, errors.New("CDP websocket handshake failed")
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, " 101 ") {
		_ = conn.Close()
		return nil, errors.New("CDP websocket upgrade was rejected")
	}
	accept := ""
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			_ = conn.Close()
			return nil, errors.New("CDP websocket headers could not be read")
		}
		if line == "\r\n" {
			break
		}
		if parts := strings.SplitN(strings.TrimSpace(line), ":", 2); len(parts) == 2 && strings.EqualFold(strings.TrimSpace(parts[0]), "Sec-WebSocket-Accept") {
			accept = strings.TrimSpace(parts[1])
		}
	}
	// Chrome is local, but still validate the RFC 6455 handshake so a random
	// HTTP service cannot be mistaken for the debugging endpoint.
	digest := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	expected := base64.StdEncoding.EncodeToString(digest[:])
	if accept != expected {
		_ = conn.Close()
		return nil, errors.New("CDP websocket handshake was invalid")
	}
	return &WebSocket{conn: conn, reader: reader}, nil
}

func (w *WebSocket) Close() error {
	if w == nil || w.conn == nil {
		return nil
	}
	return w.conn.Close()
}

func (w *WebSocket) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.nextID++
	id := w.nextID
	payload, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return nil, errors.New("CDP command could not be encoded")
	}
	if err := w.writeFrame(payload); err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = w.conn.SetReadDeadline(deadline)
	} else {
		_ = w.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	}
	for {
		frame, opcode, err := w.readFrame()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil, errors.New("CDP browser connection closed")
			}
			return nil, errors.New("CDP command failed")
		}
		if opcode == 9 {
			if err := w.writeControl(10, frame); err != nil {
				return nil, err
			}
			continue
		}
		if opcode != 1 {
			continue
		}
		var envelope struct {
			ID     int64           `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(frame, &envelope) != nil || envelope.ID != id {
			continue
		}
		if envelope.Error != nil {
			return nil, errors.New("CDP command was rejected")
		}
		return envelope.Result, nil
	}
}

func (w *WebSocket) writeControl(opcode byte, payload []byte) error {
	return w.writeFrameWithOpcode(opcode, payload)
}

func (w *WebSocket) writeFrame(payload []byte) error {
	return w.writeFrameWithOpcode(1, payload)
}

func (w *WebSocket) writeFrameWithOpcode(opcode byte, payload []byte) error {
	if len(payload) > 1<<24 {
		return errors.New("CDP websocket frame is too large")
	}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return errors.New("CDP websocket frame could not be masked")
	}
	header := []byte{0x80 | opcode}
	length := len(payload)
	switch {
	case length < 126:
		header = append(header, byte(0x80|length))
	case length <= 0xffff:
		header = append(header, 0xFE, 0, 0)
		binary.BigEndian.PutUint16(header[len(header)-2:], uint16(length))
	default:
		header = append(header, 0xFF, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(header[len(header)-8:], uint64(length))
	}
	header = append(header, mask...)
	masked := make([]byte, len(payload))
	for index, value := range payload {
		masked[index] = value ^ mask[index%4]
	}
	_, err := w.conn.Write(append(header, masked...))
	return err
}

func (w *WebSocket) readFrame() ([]byte, byte, error) {
	first, err := w.reader.ReadByte()
	if err != nil {
		return nil, 0, err
	}
	second, err := w.reader.ReadByte()
	if err != nil {
		return nil, 0, err
	}
	opcode := first & 0x0f
	length := int64(second & 0x7f)
	if length == 126 {
		var value uint16
		if err := binary.Read(w.reader, binary.BigEndian, &value); err != nil {
			return nil, 0, err
		}
		length = int64(value)
	} else if length == 127 {
		var value uint64
		if err := binary.Read(w.reader, binary.BigEndian, &value); err != nil || value > 1<<24 {
			return nil, 0, errors.New("CDP websocket frame length is invalid")
		}
		length = int64(value)
	}
	masked := second&0x80 != 0
	mask := make([]byte, 4)
	if masked {
		if _, err := io.ReadFull(w.reader, mask); err != nil {
			return nil, 0, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(w.reader, payload); err != nil {
		return nil, 0, err
	}
	if masked {
		for index := range payload {
			payload[index] ^= mask[index%4]
		}
	}
	return payload, opcode, nil
}
