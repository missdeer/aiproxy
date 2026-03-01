package proxy

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"
	"testing"
)

func buildTestFrame(eventType, messageType string, payload []byte) []byte {
	var headers bytes.Buffer

	writeStringHeader := func(name, value string) {
		headers.WriteByte(byte(len(name)))
		headers.WriteString(name)
		headers.WriteByte(7) // string type
		binary.Write(&headers, binary.BigEndian, uint16(len(value)))
		headers.WriteString(value)
	}

	if eventType != "" {
		writeStringHeader(":event-type", eventType)
	}
	if messageType != "" {
		writeStringHeader(":message-type", messageType)
	}

	headersBytes := headers.Bytes()
	totalLength := 12 + len(headersBytes) + len(payload) + 4
	frame := make([]byte, totalLength)
	binary.BigEndian.PutUint32(frame[0:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headersBytes)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[0:8]))
	copy(frame[12:12+len(headersBytes)], headersBytes)
	copy(frame[12+len(headersBytes):12+len(headersBytes)+len(payload)], payload)
	binary.BigEndian.PutUint32(frame[totalLength-4:totalLength], crc32.ChecksumIEEE(frame[:totalLength-4]))
	return frame
}

func TestReadEventStreamFrame(t *testing.T) {
	payload := []byte(`{"content":"hello"}`)
	data := buildTestFrame("assistantResponseEvent", "event", payload)

	frame, err := readEventStreamFrame(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if frame.EventType != "assistantResponseEvent" {
		t.Errorf("EventType = %q, want %q", frame.EventType, "assistantResponseEvent")
	}
	if frame.MessageType != "event" {
		t.Errorf("MessageType = %q, want %q", frame.MessageType, "event")
	}
	if string(frame.Payload) != string(payload) {
		t.Errorf("Payload = %q, want %q", frame.Payload, payload)
	}
}

func TestReadEventStreamFrameDefaultMessageType(t *testing.T) {
	payload := []byte(`{"text":"thinking"}`)
	data := buildTestFrame("reasoningContentEvent", "", payload)

	frame, err := readEventStreamFrame(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if frame.MessageType != "event" {
		t.Errorf("MessageType = %q, want default %q", frame.MessageType, "event")
	}
}

func TestReadEventStreamFrameErrorType(t *testing.T) {
	payload := []byte(`{"message":"throttled"}`)
	data := buildTestFrame("", "error", payload)

	frame, err := readEventStreamFrame(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if frame.MessageType != "error" {
		t.Errorf("MessageType = %q, want %q", frame.MessageType, "error")
	}
}

func TestReadEventStreamFrameExceptionType(t *testing.T) {
	payload := []byte(`{"message":"internal error"}`)
	data := buildTestFrame("", "exception", payload)

	frame, err := readEventStreamFrame(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if frame.MessageType != "exception" {
		t.Errorf("MessageType = %q, want %q", frame.MessageType, "exception")
	}
}

func TestReadEventStreamFrameTruncatedPrelude(t *testing.T) {
	data := []byte{0, 0, 0} // only 3 bytes
	_, err := readEventStreamFrame(bytes.NewReader(data))
	if err == nil {
		t.Fatal("expected error for truncated prelude")
	}
}

func TestReadEventStreamFrameTooSmall(t *testing.T) {
	var data [12]byte
	binary.BigEndian.PutUint32(data[0:4], 10) // totalLength < 16
	binary.BigEndian.PutUint32(data[4:8], 0)
	binary.BigEndian.PutUint32(data[8:12], 0)

	_, err := readEventStreamFrame(bytes.NewReader(data[:]))
	if err == nil {
		t.Fatal("expected error for frame too small")
	}
}

func TestReadEventStreamFrameTooLarge(t *testing.T) {
	var data [12]byte
	binary.BigEndian.PutUint32(data[0:4], uint32(maxEventStreamFrameSize+1))
	binary.BigEndian.PutUint32(data[4:8], 0)
	binary.BigEndian.PutUint32(data[8:12], 0)

	_, err := readEventStreamFrame(bytes.NewReader(data[:]))
	if err == nil {
		t.Fatal("expected error for frame too large")
	}
}

func TestReadEventStreamFrameHeadersExceedBounds(t *testing.T) {
	var data [12]byte
	binary.BigEndian.PutUint32(data[0:4], 20)  // total = 20
	binary.BigEndian.PutUint32(data[4:8], 100) // headers = 100 > 20-16=4
	binary.BigEndian.PutUint32(data[8:12], 0)

	_, err := readEventStreamFrame(bytes.NewReader(data[:]))
	if err == nil {
		t.Fatal("expected error for headers exceeding bounds")
	}
}

func TestReadEventStreamFrameEmptyPayload(t *testing.T) {
	data := buildTestFrame("assistantResponseEvent", "event", nil)

	frame, err := readEventStreamFrame(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(frame.Payload) != 0 {
		t.Errorf("expected empty payload, got %d bytes", len(frame.Payload))
	}
}

func TestReadMultipleFrames(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(buildTestFrame("assistantResponseEvent", "event", []byte(`{"content":"hello"}`)))
	buf.Write(buildTestFrame("reasoningContentEvent", "event", []byte(`{"text":"think"}`)))
	buf.Write(buildTestFrame("meteringEvent", "event", []byte(`{"usage":1.5}`)))

	r := bytes.NewReader(buf.Bytes())
	types := []string{"assistantResponseEvent", "reasoningContentEvent", "meteringEvent"}
	for i, want := range types {
		frame, err := readEventStreamFrame(r)
		if err != nil {
			t.Fatalf("frame %d: unexpected error: %v", i, err)
		}
		if frame.EventType != want {
			t.Errorf("frame %d: EventType = %q, want %q", i, frame.EventType, want)
		}
	}

	_, err := readEventStreamFrame(r)
	if err != io.EOF {
		t.Errorf("expected io.EOF after all frames, got %v", err)
	}
}

func TestReadEventStreamFrameMalformedStringHeader(t *testing.T) {
	// Header says string length=3 but only 2 bytes provided.
	headers := []byte{
		byte(len(":event-type")),
	}
	headers = append(headers, []byte(":event-type")...)
	headers = append(headers, 7)          // string type
	headers = append(headers, 0x00, 0x03) // declared length
	headers = append(headers, 'o', 'k')   // actual length 2 (truncated)

	totalLength := 12 + len(headers) + 4
	frame := make([]byte, totalLength)
	binary.BigEndian.PutUint32(frame[0:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[0:8]))
	copy(frame[12:12+len(headers)], headers)
	binary.BigEndian.PutUint32(frame[totalLength-4:], crc32.ChecksumIEEE(frame[:totalLength-4]))

	_, err := readEventStreamFrame(bytes.NewReader(frame))
	if err == nil {
		t.Fatal("expected error for malformed string header")
	}
}

func TestReadEventStreamFrameUnsupportedHeaderType(t *testing.T) {
	headers := []byte{
		byte(len(":event-type")),
	}
	headers = append(headers, []byte(":event-type")...)
	headers = append(headers, 255) // unsupported header type

	totalLength := 12 + len(headers) + 4
	frame := make([]byte, totalLength)
	binary.BigEndian.PutUint32(frame[0:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[0:8]))
	copy(frame[12:12+len(headers)], headers)
	binary.BigEndian.PutUint32(frame[totalLength-4:], crc32.ChecksumIEEE(frame[:totalLength-4]))

	_, err := readEventStreamFrame(bytes.NewReader(frame))
	if err == nil {
		t.Fatal("expected error for unsupported header value type")
	}
}

func TestReadEventStreamFrameInvalidPreludeCRC(t *testing.T) {
	payload := []byte(`{"content":"hello"}`)
	data := buildTestFrame("assistantResponseEvent", "event", payload)
	data[11] ^= 0xFF // corrupt prelude CRC

	_, err := readEventStreamFrame(bytes.NewReader(data))
	if err == nil {
		t.Fatal("expected error for invalid prelude CRC")
	}
}

func TestReadEventStreamFrameInvalidMessageCRC(t *testing.T) {
	payload := []byte(`{"content":"hello"}`)
	data := buildTestFrame("assistantResponseEvent", "event", payload)
	data[len(data)-1] ^= 0xFF // corrupt message CRC

	_, err := readEventStreamFrame(bytes.NewReader(data))
	if err == nil {
		t.Fatal("expected error for invalid message CRC")
	}
}
