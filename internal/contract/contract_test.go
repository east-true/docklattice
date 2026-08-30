package contract

import (
	"testing"

	"github.com/east-true/docklattice/internal/transport"
)

func TestAppendixA4MethodTable(t *testing.T) {
	want := map[transport.Method]Spec{
		MethodRegister:          {MethodRegister, transport.KindUnary, transport.ClassControl},
		MethodHeartbeat:         {MethodHeartbeat, transport.KindUnary, transport.ClassControl},
		MethodCancelOperation:   {MethodCancelOperation, transport.KindUnary, transport.ClassControl},
		MethodOperationProgress: {MethodOperationProgress, transport.KindReceive, transport.ClassControl},
		MethodSyncAudit:         {MethodSyncAudit, transport.KindDuplex, transport.ClassDurable},
		MethodGetAuditCoverage:  {MethodGetAuditCoverage, transport.KindUnary, transport.ClassDurable},
		MethodEcho:              {MethodEcho, transport.KindUnary, transport.ClassQuery},
		MethodStreamLogs:        {MethodStreamLogs, transport.KindReceive, transport.ClassBulk},
		MethodOperationOutput:   {MethodOperationOutput, transport.KindReceive, transport.ClassBulk},
		MethodStreamStats:       {MethodStreamStats, transport.KindReceive, transport.ClassLive},
	}
	if len(Methods()) != len(want) {
		t.Fatalf("method count = %d, want %d", len(Methods()), len(want))
	}
	for method, expected := range want {
		got, ok := SpecOf(method)
		if !ok || got != expected {
			t.Errorf("SpecOf(%s) = %+v, %t; want %+v", method, got, ok, expected)
		}
	}
}

func TestCheckKindRejectsClassLeak(t *testing.T) {
	err := checkKind(transport.Call{Method: MethodStreamLogs, Class: transport.ClassControl}, transport.KindReceive)
	if transport.StatusOf(err).Code != transport.CodeProtocol {
		t.Fatalf("status = %v", err)
	}
}
