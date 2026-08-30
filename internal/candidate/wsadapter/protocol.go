package wsadapter

import (
	"encoding/binary"
	"io"

	"github.com/east-true/docklattice/internal/transport"
)

type frameType uint8

const (
	frameOpen frameType = iota + 1
	frameData
	frameCredit
	frameClose
	frameCancel
	framePing
)

const (
	wireHeaderBytes   = 24
	initialCreditByte = 64 << 10
	// The controlled workload's smallest DATA message is a 200-byte log line.
	// Keep message credit above 64KiB/200 so the byte window—not a hidden 3.2KiB
	// message cap—is the buffer corresponding to gRPC's 64KiB initial window.
	initialCreditMsgs = 512
	maxMethodBytes    = 1024
	halfCloseAux      = 255
)

type frame struct {
	streamID   uint64
	typ        frameType
	class      transport.Class
	aux        uint8
	creditByte uint32
	creditMsgs uint32
	payload    []byte
}

func writeFrame(w io.Writer, f frame) error {
	header := make([]byte, wireHeaderBytes)
	binary.BigEndian.PutUint64(header[0:8], f.streamID)
	header[8] = byte(f.typ)
	header[9] = byte(f.class)
	header[10] = f.aux
	binary.BigEndian.PutUint32(header[12:16], uint32(len(f.payload)))
	binary.BigEndian.PutUint32(header[16:20], f.creditByte)
	binary.BigEndian.PutUint32(header[20:24], f.creditMsgs)
	if err := writeFull(w, header); err != nil {
		return err
	}
	return writeFull(w, f.payload)
}

func readFrame(r io.Reader, maxPayload int) (frame, error) {
	header := make([]byte, wireHeaderBytes)
	if _, err := io.ReadFull(r, header); err != nil {
		return frame{}, err
	}
	length := binary.BigEndian.Uint32(header[12:16])
	if uint64(length) > uint64(maxPayload) {
		return frame{}, transport.Errorf(transport.CodeMessageTooLarge, "wire payload is %d bytes; limit is %d", length, maxPayload)
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(r, payload); err != nil {
		return frame{}, err
	}
	return frame{
		streamID:   binary.BigEndian.Uint64(header[0:8]),
		typ:        frameType(header[8]),
		class:      transport.Class(header[9]),
		aux:        header[10],
		creditByte: binary.BigEndian.Uint32(header[16:20]),
		creditMsgs: binary.BigEndian.Uint32(header[20:24]),
		payload:    payload,
	}, nil
}

func encodeOpen(call transport.Call) ([]byte, error) {
	if len(call.Method) == 0 || len(call.Method) > maxMethodBytes {
		return nil, transport.Errorf(transport.CodeProtocol, "invalid method length %d", len(call.Method))
	}
	out := make([]byte, 2+len(call.Method)+len(call.Payload))
	binary.BigEndian.PutUint16(out[:2], uint16(len(call.Method)))
	copy(out[2:], call.Method)
	copy(out[2+len(call.Method):], call.Payload)
	return out, nil
}

func decodeOpen(payload []byte) (transport.Method, []byte, error) {
	if len(payload) < 2 {
		return "", nil, transport.Errorf(transport.CodeProtocol, "short OPEN payload")
	}
	n := int(binary.BigEndian.Uint16(payload[:2]))
	if n == 0 || n > maxMethodBytes || len(payload) < 2+n {
		return "", nil, transport.Errorf(transport.CodeProtocol, "invalid OPEN method length")
	}
	return transport.Method(payload[2 : 2+n]), payload[2+n:], nil
}

func writeFull(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
