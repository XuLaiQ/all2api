package cdp

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestWebSocketDialAndCall(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			done <- acceptErr
			return
		}
		defer connection.Close()
		reader := bufio.NewReader(connection)
		key := ""
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				done <- readErr
				return
			}
			if strings.HasPrefix(strings.ToLower(line), "sec-websocket-key:") {
				key = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
			}
			if line == "\r\n" {
				break
			}
		}
		digest := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, _ = io.WriteString(connection, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+base64.StdEncoding.EncodeToString(digest[:])+"\r\n\r\n")
		payload, err := readTestFrame(reader)
		if err != nil {
			done <- err
			return
		}
		var request struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(payload, &request); err != nil {
			done <- err
			return
		}
		response, _ := json.Marshal(map[string]any{"id": request.ID, "result": map[string]any{"ok": true}})
		if err := writeTestFrame(connection, response); err != nil {
			done <- err
			return
		}
		done <- nil
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, err := Dial(ctx, "ws://"+listener.Addr().String()+"/devtools/page/test")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	result, err := ws.Call(ctx, "Runtime.enable", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(result, &value); err != nil || value["ok"] != true {
		t.Fatalf("CDP result = %s/%v", result, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func readTestFrame(reader *bufio.Reader) ([]byte, error) {
	first, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	second, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	if first&0x0f != 1 {
		return nil, io.ErrUnexpectedEOF
	}
	length := int(second & 0x7f)
	if length == 126 {
		var value uint16
		if err := binary.Read(reader, binary.BigEndian, &value); err != nil {
			return nil, err
		}
		length = int(value)
	}
	mask := make([]byte, 4)
	if second&0x80 == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	if _, err := io.ReadFull(reader, mask); err != nil {
		return nil, err
	}
	value := make([]byte, length)
	if _, err := io.ReadFull(reader, value); err != nil {
		return nil, err
	}
	for index := range value {
		value[index] ^= mask[index%4]
	}
	return value, nil
}

func writeTestFrame(writer io.Writer, payload []byte) error {
	if len(payload) >= 126 {
		return io.ErrShortBuffer
	}
	_, err := writer.Write(append([]byte{0x81, byte(len(payload))}, payload...))
	return err
}
