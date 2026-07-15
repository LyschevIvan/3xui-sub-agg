package aggregator

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/LyschevIvan/3xui-sub-agg/internal/xui"
)

func TestCopyGroupsToInboundDeduplicatesAndKeepsExistingConnections(t *testing.T) {
	panel := &mutationPanel{
		clients: []xui.ClientSummary{{Email: "alice@old", SubID: "alice", Enable: true, InboundIDs: []int{9}}},
		inbound: inboundDocument(t, "vless", "main", "tcp", "reality"),
	}
	a, _, server, userID := mutationTestAggregator(t, panel)
	result, err := a.CopyGroupsToInbound(context.Background(), userID, server.ID, 9, []string{"bob", "alice", "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Added != 1 || result.AlreadyAttached != 1 || result.Failed != 0 {
		t.Fatalf("result = %+v", result)
	}
	if len(panel.addCalls) != 1 || panel.addCalls[0].payload.SubID != "bob" {
		t.Fatalf("add calls = %+v", panel.addCalls)
	}
	if len(panel.detachCalls) != 0 {
		t.Fatalf("source connections were detached: %+v", panel.detachCalls)
	}
}

func TestCopyGroupsToInboundRejectsMoreThanLimit(t *testing.T) {
	panel := &mutationPanel{inbound: inboundDocument(t, "vless", "main", "tcp", "reality")}
	a, _, server, userID := mutationTestAggregator(t, panel)
	subIDs := make([]string, 501)
	for i := range subIDs {
		subIDs[i] = fmt.Sprintf("user-%03d", i)
	}
	_, err := a.CopyGroupsToInbound(context.Background(), userID, server.ID, 9, subIDs)
	if !errors.Is(err, errInvalidMutation) {
		t.Fatalf("err = %v", err)
	}
}
