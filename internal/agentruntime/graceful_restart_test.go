package agentruntime

import (
	"context"
	"testing"

	"github.com/east-true/docklattice/internal/auditevents"
	"github.com/east-true/docklattice/internal/auditwal"
)

// A clean SIGTERM path must leave a clean close behind: the next boot may not
// claim the previous incarnation was uncertain, and no Audit may be appended
// after the clean close was recorded.
func TestGracefulCloseLeavesNoContinuityDoubtAndNoLateAudit(t *testing.T) {
	server := newCredentialServer(t)
	config := testConfig(t.TempDir(), server)
	config.DockerOpen = (&fakeDocker{containers: labelledSelf()}).opener()
	first, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.WAL().Append(context.Background(), []byte(`{"kind":"before-shutdown"}`)); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	closedTail, err := auditwal.Recover(config.WALDir, first.Startup().AgentID, config.WALOptions)
	if err != nil {
		t.Fatal(err)
	}
	if closedTail.WALTail == nil || closedTail.DurableThrough == nil ||
		*closedTail.WALTail != *closedTail.DurableThrough {
		t.Fatalf("clean close did not leave the WAL durable through its tail: %+v", closedTail)
	}

	config.JoinToken = ""
	second, err := Boot(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close(context.Background())
	if second.Startup().PreviousUnclean {
		t.Fatal("a graceful shutdown was reported as unclean")
	}
	if second.Startup().CurrentIncarnation != first.Startup().CurrentIncarnation+1 {
		t.Fatalf("incarnation = %d after %d", second.Startup().CurrentIncarnation, first.Startup().CurrentIncarnation)
	}
	result, err := second.WAL().ReadAuditFrom(context.Background(), auditwal.Cursor{Incarnation: 1, Seq: 1}, 64)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range result.Records {
		envelope, decodeErr := auditevents.Decode(record.Payload)
		if decodeErr != nil {
			continue
		}
		if envelope.Event.Kind == auditevents.KindContinuityUncertain {
			t.Fatalf("clean restart invented a continuity boundary: %+v", envelope)
		}
		// Nothing may be appended to the closed incarnation after its clean close.
		if record.Cursor.Incarnation == first.Startup().CurrentIncarnation &&
			record.Cursor.Seq > closedTail.DurableThrough.Seq {
			t.Fatalf("record %+v was appended after the clean close", record.Cursor)
		}
	}
}
