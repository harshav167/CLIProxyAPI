package proto

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
)

const (
	ConnectEndStreamFlag   byte = 0x02
	ConnectCompressionFlag byte = 0x01
	ConnectFrameHeaderSize      = 5
)

func FrameConnectMessage(data []byte, flags byte) []byte {
	frame := make([]byte, ConnectFrameHeaderSize+len(data))
	frame[0] = flags
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(data)))
	copy(frame[5:], data)
	return frame
}

func ParseConnectFrame(buf []byte) (flags byte, payload []byte, consumed int, ok bool) {
	if len(buf) < ConnectFrameHeaderSize {
		return 0, nil, 0, false
	}
	flags = buf[0]
	length := binary.BigEndian.Uint32(buf[1:5])
	total := ConnectFrameHeaderSize + int(length)
	if len(buf) < total {
		return 0, nil, 0, false
	}
	return flags, buf[5:total], total, true
}

type ConnectError struct {
	Code    string
	Message string
}

func (e *ConnectError) Error() string {
	return fmt.Sprintf("Connect error %s: %s", e.Code, e.Message)
}

func ParseConnectEndStream(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	var trailer struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &trailer); err != nil {
		return fmt.Errorf("failed to parse Connect end stream: %w", err)
	}
	if trailer.Error != nil {
		code := trailer.Error.Code
		if code == "" {
			code = "unknown"
		}
		msg := trailer.Error.Message
		if msg == "" {
			msg = "Unknown error"
		}
		return &ConnectError{Code: code, Message: msg}
	}
	return nil
}
