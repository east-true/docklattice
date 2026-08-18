package transport

import (
	"context"
	"errors"
	"testing"
)

func TestStatusRoundTrip(t *testing.T) {
	cause := errors.New("cause")
	err := Wrap(CodeUnavailable, cause, "connection lost")
	status := StatusOf(err)
	if status.Code != CodeUnavailable || status.Reason != "connection lost" || !errors.Is(err, cause) {
		t.Fatalf("StatusOf = %+v, err=%v", status, err)
	}
	if StatusOf(nil).Code != CodeOK {
		t.Fatal("nil must map to OK")
	}
}

func TestStatusOfContextErrors(t *testing.T) {
	if got := StatusOf(context.Canceled).Code; got != CodeCanceled {
		t.Fatalf("context.Canceled mapped to %s", got)
	}
	if got := StatusOf(context.DeadlineExceeded).Code; got != CodeDeadlineExceeded {
		t.Fatalf("context deadline mapped to %s", got)
	}
}

func TestClassContract(t *testing.T) {
	for class := ClassControl; class < NumClasses; class++ {
		if !class.Valid() || class.Priority() != int(class) {
			t.Fatalf("invalid class %d", class)
		}
	}
	if !ClassControl.IsProtected() || !ClassDurable.IsProtected() || ClassBulk.IsProtected() {
		t.Fatal("protected class mapping changed")
	}
	if !ClassLive.IsLatestWins() || ClassBulk.IsLatestWins() {
		t.Fatal("latest-wins mapping changed")
	}
}
