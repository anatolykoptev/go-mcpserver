package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAbortText(t *testing.T) {
	deadlineCtx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-deadlineCtx.Done()

	cancelled, cancelCtx := context.WithCancel(context.Background())
	cancelCtx()

	live := context.Background()
	goneClient, goneCancel := context.WithCancel(context.Background())
	goneCancel()

	for _, tc := range []struct {
		name      string
		ctx, cc   context.Context
		want, not string
	}{
		{"timeout", deadlineCtx, live, "timed out after", "cancelled"},
		{"timeout wins even if client also gone", deadlineCtx, goneClient, "timed out after", "disconnected"},
		{"generic cancel (notifications/cancelled, session close)", cancelled, live, `tool "t" cancelled`, "disconnected"},
		{"generic cancel without client ctx", cancelled, nil, `tool "t" cancelled`, "disconnected"},
		{"client disconnected", cancelled, goneClient, "client disconnected", "timed out"},
	} {
		got := abortText(tc.ctx, tc.cc, "t", time.Second, time.Now())
		if !strings.Contains(got, tc.want) || strings.Contains(got, tc.not) {
			t.Errorf("%s: abortText = %q; want it to contain %q and not %q", tc.name, got, tc.want, tc.not)
		}
	}
}
