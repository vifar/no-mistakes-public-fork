package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

type scriptedAdoptClient struct {
	calls  int
	params *ipc.AdoptRunLaunchParams
	err    error
	result ipc.AdoptRunLaunchResult
}

func (s *scriptedAdoptClient) CallWithContext(_ context.Context, method string, params interface{}, result interface{}, _ time.Duration) error {
	s.calls++
	if method != ipc.MethodAdoptRunLaunch {
		return errors.New("unexpected method " + method)
	}
	s.params, _ = params.(*ipc.AdoptRunLaunchParams)
	if s.err != nil {
		return s.err
	}
	*(result.(*ipc.AdoptRunLaunchResult)) = s.result
	return nil
}

// Issue #190: reattaching to a push-created run must hand the daemon the
// caller's owner and intent rather than silently keeping the run's empty ones.
func TestAdoptReattachedRunLaunchSendsOwnerAndIntent(t *testing.T) {
	client := &scriptedAdoptClient{result: ipc.AdoptRunLaunchResult{OwnerApplied: true, IntentApplied: true}}
	var notices bytes.Buffer
	if err := adoptReattachedRunLaunch(context.Background(), client, &notices, "run-1", "lane-a", "Ship it"); err != nil {
		t.Fatalf("adopt = %v", err)
	}
	if client.params == nil || client.params.RunID != "run-1" || client.params.Owner != "lane-a" || client.params.Intent != "Ship it" {
		t.Fatalf("params = %#v", client.params)
	}
	if !strings.Contains(notices.String(), "owner") || !strings.Contains(notices.String(), "intent") {
		t.Fatalf("notice %q should say what was applied", notices.String())
	}
}

func TestAdoptReattachedRunLaunchSkipsWhenNothingSupplied(t *testing.T) {
	client := &scriptedAdoptClient{}
	if err := adoptReattachedRunLaunch(context.Background(), client, &bytes.Buffer{}, "run-1", "", "  "); err != nil || client.calls != 0 {
		t.Fatalf("empty adopt err=%v calls=%d, want no call", err, client.calls)
	}
}

func TestAdoptReattachedRunLaunchRefusalIsAnError(t *testing.T) {
	for _, code := range []string{ipc.AdoptRunLaunchRefusedStarted, ipc.AdoptRunLaunchRefusedOwnerConflict, ipc.AdoptRunLaunchRefusedLaunchBound} {
		client := &scriptedAdoptClient{result: ipc.AdoptRunLaunchResult{Refused: code, Message: "run run-1 refused: " + code}}
		err := adoptReattachedRunLaunch(context.Background(), client, &bytes.Buffer{}, "run-1", "lane-a", "Ship it")
		var refusal *launchAdoptionRefusedError
		if !errors.As(err, &refusal) || refusal.Code != code || !strings.Contains(err.Error(), code) {
			t.Fatalf("refusal %s: err = %v, want a launchAdoptionRefusedError", code, err)
		}
	}
}

// An older daemon has no adopt method. Reattaching with --owner/--intent then
// fails closed with a restart instruction rather than dropping them.
func TestAdoptReattachedRunLaunchRefusesOnOlderDaemon(t *testing.T) {
	client := &scriptedAdoptClient{err: &ipc.RPCError{Code: ipc.ErrMethodNotFound, Message: "method not found: adopt_run_launch"}}
	err := adoptReattachedRunLaunch(context.Background(), client, &bytes.Buffer{}, "run-1", "lane-a", "")
	if err == nil || !strings.Contains(err.Error(), "too old") || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("older daemon err = %v, want a fail-closed restart instruction", err)
	}
	other := &scriptedAdoptClient{err: errors.New("database unavailable")}
	if err := adoptReattachedRunLaunch(context.Background(), other, &bytes.Buffer{}, "run-1", "lane-a", ""); err == nil || !strings.Contains(err.Error(), "database unavailable") {
		t.Fatalf("transport error = %v, want it surfaced", err)
	}
}
