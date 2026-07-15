package webui

import (
	"testing"

	"github.com/LyschevIvan/3xui-sub-agg/internal/aggregator"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
)

func TestBuildCatalogDeduplicatesSubscriptionsAndScopesOwner(t *testing.T) {
	family := storage.ClientGroup{ID: 5, UserID: 7, Name: "Семья", Members: []string{"alice"}}
	snapshot := &aggregator.Snapshot{Servers: []aggregator.ServerSnapshot{
		{
			ID: 1, UserID: 7, Name: "DE", PublicHost: "de.example", State: aggregator.ServerOK,
			Inbounds: []aggregator.InboundInfo{{ID: 10, Remark: "main", Port: 443, Protocol: "vless", Network: "tcp", Security: "reality", Enable: true}},
			Groups: map[string]aggregator.ClientGroup{
				"alice": {SubID: "alice", Records: []aggregator.ClientRef{{Email: "alice-de", SubID: "alice", Enabled: true, InboundIDs: []int{10}}}},
			},
		},
		{
			ID: 2, UserID: 7, Name: "FI", State: aggregator.ServerOK,
			Groups: map[string]aggregator.ClientGroup{
				"alice": {SubID: "alice", Records: []aggregator.ClientRef{{Email: "alice-fi", SubID: "alice", Enabled: true}}},
			},
		},
		{
			ID: 3, UserID: 8, Name: "Foreign", State: aggregator.ServerOK,
			Inbounds: []aggregator.InboundInfo{{ID: 30, Remark: "foreign", Protocol: "vless", Enable: true}},
			Groups:   map[string]aggregator.ClientGroup{"mallory": {SubID: "mallory"}},
		},
	}}

	got := buildCatalog(snapshot, 7, "https://sub.example/sub/token", map[string][]storage.ClientGroup{"alice": {family}})

	if len(got.Subscriptions) != 1 {
		t.Fatalf("subscriptions = %+v", got.Subscriptions)
	}
	subscription := got.Subscriptions[0]
	if subscription.SubID != "alice" || subscription.ConnectionCount != 1 || subscription.ServerCount != 1 {
		t.Fatalf("subscription = %+v", subscription)
	}
	if len(subscription.Groups) != 1 || subscription.Groups[0].Name != "Семья" {
		t.Fatalf("groups = %+v", subscription.Groups)
	}
	if len(got.Inbounds) != 1 {
		t.Fatalf("inbounds = %+v", got.Inbounds)
	}
	inbound := got.Inbounds[0]
	if inbound.ServerID != 1 || inbound.InboundID != 10 || inbound.SubscriptionCount != 1 {
		t.Fatalf("inbound = %+v", inbound)
	}
	if len(inbound.SubIDs) != 1 || inbound.SubIDs[0] != "alice" {
		t.Fatalf("inbound subscriptions = %v", inbound.SubIDs)
	}
}

func TestBuildCatalogSortsResourcesDeterministically(t *testing.T) {
	snapshot := &aggregator.Snapshot{Servers: []aggregator.ServerSnapshot{
		{
			ID: 2, UserID: 7, Name: "Zurich", State: aggregator.ServerOK,
			Inbounds: []aggregator.InboundInfo{
				{ID: 2, Remark: "zeta", Protocol: "vless", Enable: true},
				{ID: 1, Remark: "alpha", Protocol: "vless", Enable: true},
			},
			Groups: map[string]aggregator.ClientGroup{
				"z": {SubID: "z", Records: []aggregator.ClientRef{{Email: "z@example", SubID: "z"}}},
				"a": {SubID: "a", Records: []aggregator.ClientRef{{Email: "a@example", SubID: "a"}}},
			},
		},
	}}

	got := buildCatalog(snapshot, 7, "", nil)
	if got.Inbounds[0].Remark != "alpha" || got.Subscriptions[0].SubID != "a" {
		t.Fatalf("catalog not sorted: %+v %+v", got.Inbounds, got.Subscriptions)
	}
}

func TestRussianCountLabelUsesCorrectSubscriptionForms(t *testing.T) {
	tests := map[int]string{
		0:  "0 подписок",
		1:  "1 подписка",
		2:  "2 подписки",
		5:  "5 подписок",
		11: "11 подписок",
		21: "21 подписка",
		24: "24 подписки",
		25: "25 подписок",
	}

	for count, want := range tests {
		if got := subscriptionCountLabel(count); got != want {
			t.Errorf("subscriptionCountLabel(%d) = %q, want %q", count, got, want)
		}
	}
}

func TestBuildCatalogKeepsEffectivePublicHostPort(t *testing.T) {
	snapshot := &aggregator.Snapshot{Servers: []aggregator.ServerSnapshot{{
		ID: 1, UserID: 7, Name: "DE", PublicHost: "public.example:8443", State: aggregator.ServerOK,
		Inbounds: []aggregator.InboundInfo{{ID: 10, Remark: "main", Port: 443, Protocol: "vless", Enable: true}},
	}}}

	got := buildCatalog(snapshot, 7, "", nil)
	if len(got.Inbounds) != 1 {
		t.Fatalf("inbounds = %+v", got.Inbounds)
	}
	if got.Inbounds[0].Endpoint != "public.example:8443" {
		t.Fatalf("endpoint = %q, want %q", got.Inbounds[0].Endpoint, "public.example:8443")
	}
}
