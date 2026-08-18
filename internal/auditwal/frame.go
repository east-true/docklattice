package auditwal

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"time"
)

const envelopeHeaderBytes = 24

var checksumTable = crc32.MakeTable(crc32.Castagnoli)

func encodeFrame(cursor Cursor, appendedAt time.Time, payload []byte) ([]byte, error) {
	bodyLen := envelopeHeaderBytes + len(payload)
	if uint64(bodyLen) > uint64(^uint32(0)) {
		return nil, ErrRecordTooLarge
	}
	frame := make([]byte, 4+bodyLen+4)
	binary.BigEndian.PutUint32(frame[:4], uint32(bodyLen))
	body := frame[4 : 4+bodyLen]
	binary.BigEndian.PutUint64(body[0:8], cursor.Incarnation)
	binary.BigEndian.PutUint64(body[8:16], cursor.Seq)
	binary.BigEndian.PutUint64(body[16:24], uint64(appendedAt.UnixNano()))
	copy(body[24:], payload)
	binary.BigEndian.PutUint32(frame[4+bodyLen:], crc32.Checksum(body, checksumTable))
	return frame, nil
}

type decodedFrame struct {
	cursor  Cursor
	at      time.Time
	payload []byte
	bytes   int64
}

func readFrame(reader io.Reader, maxFrameBytes int64) (decodedFrame, error) {
	var lengthBytes [4]byte
	if _, err := io.ReadFull(reader, lengthBytes[:]); err != nil {
		return decodedFrame{}, err
	}
	bodyLen := int64(binary.BigEndian.Uint32(lengthBytes[:]))
	if bodyLen < envelopeHeaderBytes || bodyLen+8 > maxFrameBytes {
		return decodedFrame{}, fmt.Errorf("%w: invalid frame length %d", ErrCorrupt, bodyLen)
	}
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(reader, body); err != nil {
		return decodedFrame{}, err
	}
	var checksumBytes [4]byte
	if _, err := io.ReadFull(reader, checksumBytes[:]); err != nil {
		return decodedFrame{}, err
	}
	want := binary.BigEndian.Uint32(checksumBytes[:])
	if got := crc32.Checksum(body, checksumTable); got != want {
		return decodedFrame{}, fmt.Errorf("%w: checksum mismatch", ErrCorrupt)
	}
	incarnation := binary.BigEndian.Uint64(body[0:8])
	seq := binary.BigEndian.Uint64(body[8:16])
	if incarnation == 0 || seq == 0 {
		return decodedFrame{}, fmt.Errorf("%w: zero cursor component", ErrCorrupt)
	}
	nanos := int64(binary.BigEndian.Uint64(body[16:24]))
	return decodedFrame{
		cursor: Cursor{Incarnation: incarnation, Seq: seq},
		at:     time.Unix(0, nanos).UTC(), payload: append([]byte(nil), body[24:]...),
		bytes: 4 + bodyLen + 4,
	}, nil
}

func writeFull(file *os.File, data []byte) error {
	for len(data) > 0 {
		written, err := file.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
