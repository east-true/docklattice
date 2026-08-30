package wsadapter

import (
	"bytes"
	"testing"

	"github.com/east-true/docklattice/internal/transport"
)

func TestFrameRoundTrip(t *testing.T) {
	want := frame{streamID: 42, typ: frameCredit, class: transport.ClassDurable, aux: 3, creditByte: 65536, creditMsgs: 16, payload: []byte("payload")}
	var buf bytes.Buffer
	if err := writeFrame(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err := readFrame(&buf, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if got.streamID != want.streamID || got.typ != want.typ || got.class != want.class || got.aux != want.aux || got.creditByte != want.creditByte || got.creditMsgs != want.creditMsgs || !bytes.Equal(got.payload, want.payload) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestControlledLogCreditMatchesByteWindow(t *testing.T) {
	const controlledLogLineBytes = 200
	if initialCreditMsgs*controlledLogLineBytes < initialCreditByte {
		t.Fatalf("message credit limits controlled logs to %d bytes before the %d-byte window", initialCreditMsgs*controlledLogLineBytes, initialCreditByte)
	}
}

func TestOpenRoundTrip(t *testing.T) {
	call := transport.Call{Method: "method", Class: transport.ClassQuery, Payload: []byte("request")}
	b, err := encodeOpen(call)
	if err != nil {
		t.Fatal(err)
	}
	method, payload, err := decodeOpen(b)
	if err != nil || method != call.Method || !bytes.Equal(payload, call.Payload) {
		t.Fatalf("decode = %s %q %v", method, payload, err)
	}
}
