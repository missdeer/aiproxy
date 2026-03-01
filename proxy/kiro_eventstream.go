package proxy

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
)

const maxEventStreamFrameSize = 16 << 20 // 16 MB

type eventStreamFrame struct {
	EventType   string
	MessageType string // "event", "error", "exception"
	Payload     []byte
}

func readEventStreamFrame(r io.Reader) (*eventStreamFrame, error) {
	var prelude [12]byte
	if _, err := io.ReadFull(r, prelude[:]); err != nil {
		return nil, err
	}

	totalLength := int(binary.BigEndian.Uint32(prelude[0:4]))
	headersLength := int(binary.BigEndian.Uint32(prelude[4:8]))

	if totalLength < 16 {
		return nil, fmt.Errorf("event stream frame too small: %d", totalLength)
	}
	if totalLength > maxEventStreamFrameSize {
		return nil, fmt.Errorf("event stream frame too large: %d", totalLength)
	}
	if headersLength > totalLength-16 {
		return nil, fmt.Errorf("headers length %d exceeds frame bounds (total %d)", headersLength, totalLength)
	}
	expectedPreludeCRC := binary.BigEndian.Uint32(prelude[8:12])
	actualPreludeCRC := crc32.ChecksumIEEE(prelude[0:8])
	if expectedPreludeCRC != actualPreludeCRC {
		return nil, fmt.Errorf("invalid prelude crc: expected %08x got %08x", expectedPreludeCRC, actualPreludeCRC)
	}

	remaining := totalLength - 12
	buf := make([]byte, remaining)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	expectedMessageCRC := binary.BigEndian.Uint32(buf[len(buf)-4:])
	crc := crc32.NewIEEE()
	crc.Write(prelude[:])
	crc.Write(buf[:len(buf)-4])
	actualMessageCRC := crc.Sum32()
	if expectedMessageCRC != actualMessageCRC {
		return nil, fmt.Errorf("invalid message crc: expected %08x got %08x", expectedMessageCRC, actualMessageCRC)
	}

	frame := &eventStreamFrame{MessageType: "event"}
	if err := parseEventStreamHeaders(buf[:headersLength], frame); err != nil {
		return nil, fmt.Errorf("parse event stream headers: %w", err)
	}

	payloadEnd := headersLength + (remaining - headersLength - 4)
	if payloadEnd > headersLength {
		frame.Payload = buf[headersLength:payloadEnd]
	}

	return frame, nil
}

func parseEventStreamHeaders(data []byte, frame *eventStreamFrame) error {
	offset := 0
	for offset < len(data) {
		nameLen := int(data[offset])
		offset++
		if offset+nameLen > len(data) {
			return fmt.Errorf("truncated header name")
		}
		name := string(data[offset : offset+nameLen])
		offset += nameLen

		if offset >= len(data) {
			return fmt.Errorf("missing header value type")
		}
		valueType := data[offset]
		offset++

		switch valueType {
		case 7: // String
			if offset+2 > len(data) {
				return fmt.Errorf("truncated string header length")
			}
			valueLen := int(binary.BigEndian.Uint16(data[offset : offset+2]))
			offset += 2
			if offset+valueLen > len(data) {
				return fmt.Errorf("truncated string header value")
			}
			value := string(data[offset : offset+valueLen])
			offset += valueLen

			switch name {
			case ":event-type":
				frame.EventType = value
			case ":message-type":
				frame.MessageType = value
			}
		case 6: // Bytes
			if offset+2 > len(data) {
				return fmt.Errorf("truncated bytes header length")
			}
			valueLen := int(binary.BigEndian.Uint16(data[offset : offset+2]))
			offset += 2
			if offset+valueLen > len(data) {
				return fmt.Errorf("truncated bytes header value")
			}
			offset += valueLen
		case 0, 1: // bool true/false
			// no value bytes
		case 2: // byte
			if offset+1 > len(data) {
				return fmt.Errorf("truncated byte header value")
			}
			offset += 1
		case 3: // short
			if offset+2 > len(data) {
				return fmt.Errorf("truncated short header value")
			}
			offset += 2
		case 4: // int
			if offset+4 > len(data) {
				return fmt.Errorf("truncated int header value")
			}
			offset += 4
		case 5: // long
			if offset+8 > len(data) {
				return fmt.Errorf("truncated long header value")
			}
			offset += 8
		case 8: // timestamp
			if offset+8 > len(data) {
				return fmt.Errorf("truncated timestamp header value")
			}
			offset += 8
		case 9: // uuid
			if offset+16 > len(data) {
				return fmt.Errorf("truncated uuid header value")
			}
			offset += 16
		default:
			return fmt.Errorf("unsupported header value type %d", valueType)
		}
	}
	return nil
}
