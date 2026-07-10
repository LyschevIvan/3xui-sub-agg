package aggregator

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LyschevIvan/3xui-sub-agg/internal/config"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
	"github.com/LyschevIvan/3xui-sub-agg/internal/xui"
)

type inboundMutationPanel struct {
	mu sync.Mutex

	documents map[int]xui.InboundDocument
	clients   []xui.ClientSummary
	details   map[string]xui.ClientDetail
	createdID int

	getCalls       []int
	listCalls      int
	slimCalls      int
	addInbound     []xui.InboundDocument
	updateInbound  []xui.InboundDocument
	updateIDs      []int
	deleteInbounds []int
	addClients     []mutationCall
	attachClients  []mutationCall
	deleteClients  []string

	updateErr        error
	deleteInboundErr error
	addClientErrFor  map[string]error
	attachErrFor     map[string]error
	deleteClientErr  map[string]error
	validateErr      error
	addClientBlock   chan struct{}
	addClientStarted chan struct{}
	afterGet         func()
	afterUpdate      func()
}

func (p *inboundMutationPanel) Validate(context.Context) (xui.ServerStatus, error) {
	return xui.ServerStatus{PanelVersion: "3.4.2"}, p.validateErr
}

func (p *inboundMutationPanel) ListClients(context.Context) ([]xui.ClientSummary, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listCalls++
	return slices.Clone(p.clients), nil
}

func (p *inboundMutationPanel) GetClient(_ context.Context, email string) (xui.ClientDetail, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.details[email], nil
}

func (p *inboundMutationPanel) AddClient(ctx context.Context, payload xui.ClientPayload, inboundIDs []int) error {
	p.mu.Lock()
	p.addClients = append(p.addClients, mutationCall{email: payload.Email, payload: payload, inboundIDs: slices.Clone(inboundIDs)})
	block, started, err := p.addClientBlock, p.addClientStarted, p.addClientErrFor[payload.SubID]
	p.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.clients = append(p.clients, xui.ClientSummary{Email: payload.Email, SubID: payload.SubID, Enable: payload.Enable, InboundIDs: slices.Clone(inboundIDs)})
	p.details[payload.Email] = xui.ClientDetail{Client: xui.ClientRecord{Email: payload.Email, SubID: payload.SubID, UUID: payload.ID}}
	p.mu.Unlock()
	return nil
}

func (*inboundMutationPanel) UpdateClient(context.Context, string, xui.ClientPayload) error {
	return nil
}

func (p *inboundMutationPanel) DeleteClient(_ context.Context, email string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleteClients = append(p.deleteClients, email)
	return p.deleteClientErr[email]
}

func (p *inboundMutationPanel) AttachClient(_ context.Context, email string, inboundIDs []int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attachClients = append(p.attachClients, mutationCall{email: email, inboundIDs: slices.Clone(inboundIDs)})
	if err := p.attachErrFor[email]; err != nil {
		return err
	}
	for i := range p.clients {
		if p.clients[i].Email == email {
			p.clients[i].InboundIDs = append(p.clients[i].InboundIDs, inboundIDs...)
		}
	}
	return nil
}

func (*inboundMutationPanel) DetachClient(context.Context, string, []int) error { return nil }
func (*inboundMutationPanel) SubLinks(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func (p *inboundMutationPanel) ListSlimInbounds(context.Context) ([]xui.InboundSummary, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.slimCalls++
	return nil, nil
}

func (p *inboundMutationPanel) GetInbound(_ context.Context, id int) (xui.InboundDocument, error) {
	p.mu.Lock()
	p.getCalls = append(p.getCalls, id)
	doc := p.documents[id].Clone()
	after := p.afterGet
	p.mu.Unlock()
	if after != nil {
		after()
	}
	return doc, nil
}

func (p *inboundMutationPanel) AddInbound(_ context.Context, doc xui.InboundDocument) (xui.InboundDocument, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.addInbound = append(p.addInbound, doc.Clone())
	created := doc.Clone()
	_ = created.Set("id", p.createdID)
	p.documents[p.createdID] = created.Clone()
	return created, nil
}

func (p *inboundMutationPanel) UpdateInbound(_ context.Context, id int, doc xui.InboundDocument) (xui.InboundDocument, error) {
	p.mu.Lock()
	p.updateIDs = append(p.updateIDs, id)
	p.updateInbound = append(p.updateInbound, doc.Clone())
	err, after := p.updateErr, p.afterUpdate
	p.mu.Unlock()
	if after != nil {
		after()
	}
	return doc.Clone(), err
}

func (p *inboundMutationPanel) DeleteInbound(_ context.Context, id int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleteInbounds = append(p.deleteInbounds, id)
	return p.deleteInboundErr
}

func inboundMutationAggregator(t *testing.T, panels map[int64]*inboundMutationPanel) (*Aggregator, *storage.Store, storage.Server, int64) {
	t.Helper()
	store, user, _ := testStore(t)
	server := createServer(t, store, user.ID, "source", "source-token")
	a := New(&config.Config{RequestTimeout: time.Second, RefreshInterval: time.Hour}, store)
	a.panelFactory = func(srv storage.Server) (xui.PanelAPI, error) { return panels[srv.ID], nil }
	return a, store, server, user.ID
}

func mustInboundDocument(t *testing.T, raw string) xui.InboundDocument {
	t.Helper()
	var doc xui.InboundDocument
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestEditInboundFetchesFreshAndPreservesUnknownFields(t *testing.T) {
	source := mustInboundDocument(t, `{
		"id":9,"up":5,"down":6,"remark":"old","port":443,"enable":false,"tag":"old-tag","protocol":"vless",
		"unknownTop":{"keep":[1,2]},
		"settings":{"clients":[],"decryption":"none","unknownSetting":{"keep":true}},
		"streamSettings":{"network":"tcp","security":"reality","realitySettings":{"show":false,"unknown":"keep"}},
		"sniffing":{"enabled":true,"destOverride":["http"],"unknownSniff":7}
	}`)
	panel := &inboundMutationPanel{documents: map[int]xui.InboundDocument{9: source}, details: map[string]xui.ClientDetail{}}
	a, _, server, userID := inboundMutationAggregator(t, map[int64]*inboundMutationPanel{1: panel})

	err := a.EditInbound(context.Background(), userID, server.ID, 9, InboundPatch{Remark: "new", Port: 8443, Enable: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(panel.getCalls, []int{9}) || len(panel.updateInbound) != 1 || !slices.Equal(panel.updateIDs, []int{9}) {
		t.Fatalf("get=%v updateIDs=%v updates=%d", panel.getCalls, panel.updateIDs, len(panel.updateInbound))
	}
	want := source.Clone()
	_ = want.Set("remark", "new")
	_ = want.Set("port", 8443)
	_ = want.Set("enable", true)
	_ = want.Set("tag", "inbound-8443")
	if !reflect.DeepEqual(panel.updateInbound[0], want) {
		gotJSON, _ := json.Marshal(panel.updateInbound[0])
		wantJSON, _ := json.Marshal(want)
		t.Fatalf("lossy update\ngot=%s\nwant=%s", gotJSON, wantJSON)
	}
	if panel.slimCalls != 1 { // exactly the required post-mutation refresh, never mutation input
		t.Fatalf("slim calls=%d", panel.slimCalls)
	}
}

func TestEditInboundRejectsInvalidOwnershipProtocolAndStaleGeneration(t *testing.T) {
	panel := &inboundMutationPanel{documents: map[int]xui.InboundDocument{9: mustInboundDocument(t, `{"id":9,"protocol":"trojan"}`)}, details: map[string]xui.ClientDetail{}}
	a, store, server, userID := inboundMutationAggregator(t, map[int64]*inboundMutationPanel{1: panel})
	foreign, _ := store.CreateUser("foreign-inbound", "unused", false)
	for _, tc := range []struct {
		name          string
		user, server  int64
		inbound, port int
	}{
		{"foreign", foreign.ID, server.ID, 9, 443}, {"bad inbound", userID, server.ID, 0, 443}, {"bad port", userID, server.ID, 9, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := a.EditInbound(context.Background(), tc.user, tc.server, tc.inbound, InboundPatch{Port: tc.port}); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	if err := a.EditInbound(context.Background(), userID, server.ID, 9, InboundPatch{Port: 443}); err == nil {
		t.Fatal("non-VLESS edit succeeded")
	}
	if len(panel.updateInbound) != 0 {
		t.Fatalf("updates=%d", len(panel.updateInbound))
	}

	panel.documents[9] = mustInboundDocument(t, `{"id":9,"protocol":"vless"}`)
	panel.afterGet = func() {
		fresh, _ := store.ServerByID(userID, server.ID)
		fresh.APIToken = "rotated"
		_ = store.UpdateServer(fresh)
	}
	if err := a.EditInbound(context.Background(), userID, server.ID, 9, InboundPatch{Port: 443}); !errors.Is(err, errStaleConnection) {
		t.Fatalf("stale err=%v", err)
	}
	if len(panel.updateInbound) != 0 {
		t.Fatalf("stale update POSTs=%d", len(panel.updateInbound))
	}
}

func TestEditInboundUpdateFailureIsExactlyOnePOST(t *testing.T) {
	panel := &inboundMutationPanel{documents: map[int]xui.InboundDocument{9: mustInboundDocument(t, `{"id":9,"protocol":"vless"}`)}, details: map[string]xui.ClientDetail{}, updateErr: errors.New("Bearer secret raw body")}
	a, _, server, userID := inboundMutationAggregator(t, map[int64]*inboundMutationPanel{1: panel})
	err := a.EditInbound(context.Background(), userID, server.ID, 9, InboundPatch{Remark: "x", Port: 443, Enable: true})
	if err == nil || strings.Contains(err.Error(), "secret") || len(panel.updateInbound) != 1 {
		t.Fatalf("err=%v posts=%d", err, len(panel.updateInbound))
	}
}

func TestDeleteInboundUsesOneNativePOSTAndKeepsOutcomeWhenRefreshFails(t *testing.T) {
	panel := &inboundMutationPanel{documents: map[int]xui.InboundDocument{}, details: map[string]xui.ClientDetail{}, validateErr: errors.New("refresh raw failure")}
	a, _, server, userID := inboundMutationAggregator(t, map[int64]*inboundMutationPanel{1: panel})
	if err := a.DeleteInbound(context.Background(), userID, server.ID, 9); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(panel.deleteInbounds, []int{9}) {
		t.Fatalf("delete calls=%v", panel.deleteInbounds)
	}
	if got := a.Snapshot().Servers; len(got) != 1 || got[0].State != ServerUnavailable {
		t.Fatalf("snapshot=%+v", got)
	}
}

func TestCopyInboundCreatesEmptyDocumentThenAttachesOrAddsClients(t *testing.T) {
	sourceDoc := mustInboundDocument(t, `{
		"id":9,"up":11,"down":12,"clientStats":[{"unknown":true}],"remark":"source","port":443,"enable":true,"tag":"old","protocol":"vless","unknownTop":"keep",
		"settings":{"clients":[{"id":"old-a","email":"source-a","subId":"alpha"},{"id":"old-b","email":"source-b","subId":"beta"}],"decryption":"none","unknownSetting":{"keep":1}},
		"streamSettings":{"network":"tcp","security":"reality","unknownStream":[1,2]},"sniffing":{"enabled":true,"unknown":9}
	}`)
	source := &inboundMutationPanel{
		documents: map[int]xui.InboundDocument{9: sourceDoc}, details: map[string]xui.ClientDetail{},
		clients: []xui.ClientSummary{{RecordID: intPtr(2), Email: "source-a", SubID: "alpha", InboundIDs: []int{9}}, {RecordID: intPtr(3), Email: "source-b", SubID: "beta", InboundIDs: []int{9}}},
	}
	source.details["source-a"] = xui.ClientDetail{Client: xui.ClientRecord{RecordID: 2, Email: "source-a", SubID: "alpha", UUID: "old-a", Security: "auto", LimitIP: 2, TotalGB: 100, ExpiryTime: 200, Enable: true, Comment: "keep-a", CreatedAt: 10, UpdatedAt: 11}}
	source.details["source-b"] = xui.ClientDetail{Client: xui.ClientRecord{RecordID: 3, Email: "source-b", SubID: "beta", UUID: "old-b", Security: "auto", LimitIP: 3, TotalGB: 300, ExpiryTime: 400, Enable: true, Comment: "keep-b", CreatedAt: 12, UpdatedAt: 13}}
	target := &inboundMutationPanel{documents: map[int]xui.InboundDocument{}, details: map[string]xui.ClientDetail{}, createdID: 77,
		clients: []xui.ClientSummary{{RecordID: intPtr(5), Email: "existing-alpha", SubID: "alpha"}},
	}
	a, store, sourceServer, userID := inboundMutationAggregator(t, map[int64]*inboundMutationPanel{1: source})
	targetServer := createServer(t, store, userID, "target", "target-token")
	a.panelFactory = func(srv storage.Server) (xui.PanelAPI, error) {
		if srv.ID == sourceServer.ID {
			return source, nil
		}
		return target, nil
	}

	createdID, err := a.CopyInbound(context.Background(), CopyInboundRequest{UserID: userID, SourceServerID: sourceServer.ID, SourceInboundID: 9, TargetServerID: targetServer.ID, Remark: "copy", Port: 8443})
	if err != nil {
		t.Fatal(err)
	}
	if createdID != 77 || len(target.addInbound) != 1 {
		t.Fatalf("created=%d adds=%d", createdID, len(target.addInbound))
	}
	clone := target.addInbound[0]
	for _, key := range []string{"id", "up", "down", "clientStats"} {
		if _, ok := clone[key]; ok {
			t.Fatalf("server-owned field %q retained", key)
		}
	}
	if got, _ := clone.String("remark"); got != "copy" {
		t.Fatalf("remark=%q", got)
	}
	if got, _ := clone.Int("port"); got != 8443 {
		t.Fatalf("port=%d", got)
	}
	if got, _ := clone.String("tag"); got != "inbound-8443" {
		t.Fatalf("tag=%q", got)
	}
	if clients, err := clone.Clients(); err != nil || len(clients) != 0 {
		t.Fatalf("clients=%+v err=%v", clients, err)
	}
	if _, ok := clone["unknownTop"]; !ok {
		t.Fatal("unknown top-level field lost")
	}
	var settings, stream map[string]json.RawMessage
	_ = json.Unmarshal(clone["settings"], &settings)
	_ = json.Unmarshal(clone["streamSettings"], &stream)
	if _, ok := settings["unknownSetting"]; !ok {
		t.Fatal("unknown settings lost")
	}
	if _, ok := stream["unknownStream"]; !ok {
		t.Fatal("unknown stream settings lost")
	}
	if len(target.attachClients) != 1 || target.attachClients[0].email != "existing-alpha" || !slices.Equal(target.attachClients[0].inboundIDs, []int{77}) {
		t.Fatalf("attach=%+v", target.attachClients)
	}
	if len(target.addClients) != 1 {
		t.Fatalf("add clients=%+v", target.addClients)
	}
	added := target.addClients[0]
	if added.payload.SubID != "beta" || added.payload.ID == "old-b" || added.payload.Email != "beta-copy" || added.payload.Flow != "xtls-rprx-vision" || added.payload.LimitIP != 3 || added.payload.TotalGB != 300 || added.payload.ExpiryTime != 400 || added.payload.Comment != "keep-b" || added.payload.CreatedAt != 0 || added.payload.UpdatedAt != 0 || !slices.Equal(added.inboundIDs, []int{77}) {
		t.Fatalf("payload=%+v ids=%v", added.payload, added.inboundIDs)
	}
	if ok, _ := regexp.MatchString(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, added.payload.ID); !ok {
		t.Fatalf("UUID=%q", added.payload.ID)
	}
}

func TestCopyInboundDeduplicatesExactSubIDsWithDeterministicCanonicalSource(t *testing.T) {
	sourceDoc := mustInboundDocument(t, `{"id":9,"remark":"source","port":443,"protocol":"vless","settings":{"clients":[{"email":"z","subId":"alpha"},{"email":"a","subId":"alpha"},{"email":"empty","subId":""}]},"streamSettings":{"network":"ws","security":"tls"}}`)
	source := &inboundMutationPanel{
		documents: map[int]xui.InboundDocument{9: sourceDoc},
		details: map[string]xui.ClientDetail{
			"z": {Client: xui.ClientRecord{RecordID: 7, Email: "z", UUID: "old-z", SubID: "alpha", Security: "auto", TotalGB: 700, Enable: true}},
			"a": {Client: xui.ClientRecord{RecordID: 2, Email: "a", UUID: "old-a", SubID: "alpha", Security: "auto", TotalGB: 200, Enable: true}},
		},
		clients: []xui.ClientSummary{{RecordID: intPtr(7), Email: "z", SubID: "alpha", InboundIDs: []int{9}}, {RecordID: intPtr(2), Email: "a", SubID: "alpha", InboundIDs: []int{9}}},
	}
	target := &inboundMutationPanel{documents: map[int]xui.InboundDocument{}, details: map[string]xui.ClientDetail{}, createdID: 77}
	a, store, sourceServer, userID := inboundMutationAggregator(t, map[int64]*inboundMutationPanel{1: source})
	targetServer := createServer(t, store, userID, "target", "target-token")
	a.panelFactory = func(srv storage.Server) (xui.PanelAPI, error) {
		if srv.ID == sourceServer.ID {
			return source, nil
		}
		return target, nil
	}

	_, err := a.CopyInbound(context.Background(), CopyInboundRequest{UserID: userID, SourceServerID: sourceServer.ID, SourceInboundID: 9, TargetServerID: targetServer.ID, Remark: "copy", Port: 8443})
	if err != nil {
		t.Fatal(err)
	}
	if len(target.addClients) != 1 || target.addClients[0].payload.SubID != "alpha" || target.addClients[0].payload.TotalGB != 200 {
		t.Fatalf("canonical/deduplicated add=%+v", target.addClients)
	}
}

func TestCopyInboundSamePanelReusesRecordAndRejectsSamePort(t *testing.T) {
	sourceDoc := mustInboundDocument(t, `{"id":9,"remark":"source","port":443,"protocol":"vless","settings":{"clients":[{"email":"one","subId":"alpha"}]},"streamSettings":{"network":"tcp","security":"none"}}`)
	panel := &inboundMutationPanel{
		documents: map[int]xui.InboundDocument{9: sourceDoc}, createdID: 77,
		details: map[string]xui.ClientDetail{"one": {Client: xui.ClientRecord{RecordID: 1, Email: "one", UUID: "old", SubID: "alpha", Security: "auto", Enable: true}}},
		clients: []xui.ClientSummary{{RecordID: intPtr(1), Email: "one", SubID: "alpha", InboundIDs: []int{9}}},
	}
	a, _, server, userID := inboundMutationAggregator(t, map[int64]*inboundMutationPanel{1: panel})

	if _, err := a.CopyInbound(context.Background(), CopyInboundRequest{UserID: userID, SourceServerID: server.ID, SourceInboundID: 9, TargetServerID: server.ID, Remark: "same", Port: 443}); err == nil {
		t.Fatal("same-server same-port copy succeeded")
	}
	if len(panel.addInbound) != 0 {
		t.Fatalf("same-port mutations=%d", len(panel.addInbound))
	}
	created, err := a.CopyInbound(context.Background(), CopyInboundRequest{UserID: userID, SourceServerID: server.ID, SourceInboundID: 9, TargetServerID: server.ID, Remark: "copy", Port: 8443})
	if err != nil || created != 77 {
		t.Fatalf("created=%d err=%v", created, err)
	}
	if len(panel.addClients) != 0 || len(panel.attachClients) != 1 || panel.attachClients[0].email != "one" {
		t.Fatalf("same-panel add=%+v attach=%+v", panel.addClients, panel.attachClients)
	}
}

func TestCopyInboundRollsBackCreatedClientsAndInbound(t *testing.T) {
	sourceDoc := mustInboundDocument(t, `{"id":9,"remark":"source","port":443,"protocol":"vless","settings":{"clients":[{"email":"one","subId":"alpha"},{"email":"two","subId":"beta"},{"email":"three","subId":"gamma"}]},"streamSettings":{"network":"ws","security":"tls"}}`)
	source := &inboundMutationPanel{documents: map[int]xui.InboundDocument{9: sourceDoc}, details: map[string]xui.ClientDetail{}, clients: []xui.ClientSummary{{RecordID: intPtr(1), Email: "one", SubID: "alpha", InboundIDs: []int{9}}, {RecordID: intPtr(2), Email: "two", SubID: "beta", InboundIDs: []int{9}}, {RecordID: intPtr(3), Email: "three", SubID: "gamma", InboundIDs: []int{9}}}}
	source.details["one"] = xui.ClientDetail{Client: xui.ClientRecord{RecordID: 1, Email: "one", UUID: "old-one", SubID: "alpha", Security: "auto", Enable: true}}
	source.details["two"] = xui.ClientDetail{Client: xui.ClientRecord{RecordID: 2, Email: "two", UUID: "old-two", SubID: "beta", Security: "auto", Enable: true}}
	source.details["three"] = xui.ClientDetail{Client: xui.ClientRecord{RecordID: 3, Email: "three", UUID: "old-three", SubID: "gamma", Security: "auto", Enable: true}}
	target := &inboundMutationPanel{documents: map[int]xui.InboundDocument{}, details: map[string]xui.ClientDetail{}, createdID: 77, clients: []xui.ClientSummary{{RecordID: intPtr(9), Email: "existing-gamma", SubID: "gamma"}}, attachErrFor: map[string]error{"existing-gamma": errors.New("Bearer raw-secret")}, deleteClientErr: map[string]error{"beta-copy": errors.New("cleanup raw-secret")}}
	a, store, sourceServer, userID := inboundMutationAggregator(t, map[int64]*inboundMutationPanel{1: source})
	targetServer := createServer(t, store, userID, "target", "target-token")
	a.panelFactory = func(srv storage.Server) (xui.PanelAPI, error) {
		if srv.ID == sourceServer.ID {
			return source, nil
		}
		return target, nil
	}

	_, err := a.CopyInbound(context.Background(), CopyInboundRequest{UserID: userID, SourceServerID: sourceServer.ID, SourceInboundID: 9, TargetServerID: targetServer.ID, Remark: "copy", Port: 8443})
	if err == nil || strings.Contains(err.Error(), "raw-secret") {
		t.Fatalf("unsafe err=%v", err)
	}
	if !slices.Equal(target.deleteClients, []string{"beta-copy", "alpha-copy"}) || !slices.Equal(target.deleteInbounds, []int{77}) {
		t.Fatalf("client cleanup=%v inbound cleanup=%v", target.deleteClients, target.deleteInbounds)
	}
	if slices.Contains(target.deleteClients, "existing-gamma") || len(target.attachClients) != 1 {
		t.Fatalf("preexisting record modified during rollback: delete=%v attach=%v", target.deleteClients, target.attachClients)
	}
}

func TestCopyInboundDoesNotDeleteClientRejectedByAdd(t *testing.T) {
	sourceDoc := mustInboundDocument(t, `{"id":9,"remark":"source","port":443,"protocol":"vless","settings":{"clients":[{"email":"one","subId":"alpha"}]},"streamSettings":{"network":"ws","security":"tls"}}`)
	source := &inboundMutationPanel{
		documents: map[int]xui.InboundDocument{9: sourceDoc},
		details: map[string]xui.ClientDetail{"one": {Client: xui.ClientRecord{
			RecordID: 1, Email: "one", UUID: "old-one", SubID: "alpha", Security: "auto", Enable: true,
		}}},
		clients: []xui.ClientSummary{{RecordID: intPtr(1), Email: "one", SubID: "alpha", InboundIDs: []int{9}}},
	}
	target := &inboundMutationPanel{
		documents: map[int]xui.InboundDocument{}, details: map[string]xui.ClientDetail{}, createdID: 77,
		addClientErrFor: map[string]error{"alpha": errors.New("client rejected")},
	}
	a, store, sourceServer, userID := inboundMutationAggregator(t, map[int64]*inboundMutationPanel{1: source})
	targetServer := createServer(t, store, userID, "target", "target-token")
	a.panelFactory = func(srv storage.Server) (xui.PanelAPI, error) {
		if srv.ID == sourceServer.ID {
			return source, nil
		}
		return target, nil
	}

	_, err := a.CopyInbound(context.Background(), CopyInboundRequest{
		UserID: userID, SourceServerID: sourceServer.ID, SourceInboundID: 9,
		TargetServerID: targetServer.ID, Remark: "copy", Port: 8443,
	})
	if err == nil {
		t.Fatal("expected copy failure")
	}
	if len(target.deleteClients) != 0 {
		t.Fatalf("rejected client must not be treated as operation-created: %v", target.deleteClients)
	}
	if !slices.Equal(target.deleteInbounds, []int{77}) {
		t.Fatalf("created inbound cleanup=%v", target.deleteInbounds)
	}
}

func TestCopyInboundAndAttachGroupShareTargetSubIDGate(t *testing.T) {
	sourceDoc := mustInboundDocument(t, `{"id":9,"remark":"source","port":443,"protocol":"vless","settings":{"clients":[{"email":"one","subId":"alpha"}]},"streamSettings":{"network":"tcp","security":"none"}}`)
	source := &inboundMutationPanel{documents: map[int]xui.InboundDocument{9: sourceDoc}, details: map[string]xui.ClientDetail{"one": {Client: xui.ClientRecord{RecordID: 1, Email: "one", UUID: "old", SubID: "alpha", Security: "auto", Enable: true}}}, clients: []xui.ClientSummary{{RecordID: intPtr(1), Email: "one", SubID: "alpha", InboundIDs: []int{9}}}}
	target := &inboundMutationPanel{documents: map[int]xui.InboundDocument{}, details: map[string]xui.ClientDetail{}, createdID: 77, addClientBlock: make(chan struct{}), addClientStarted: make(chan struct{}, 1)}
	a, store, sourceServer, userID := inboundMutationAggregator(t, map[int64]*inboundMutationPanel{1: source})
	targetServer := createServer(t, store, userID, "target", "target-token")
	a.panelFactory = func(srv storage.Server) (xui.PanelAPI, error) {
		if srv.ID == sourceServer.ID {
			return source, nil
		}
		return target, nil
	}
	copyDone := make(chan error, 1)
	go func() {
		_, err := a.CopyInbound(context.Background(), CopyInboundRequest{UserID: userID, SourceServerID: sourceServer.ID, SourceInboundID: 9, TargetServerID: targetServer.ID, Remark: "copy", Port: 8443})
		copyDone <- err
	}()
	<-target.addClientStarted
	attachDone := make(chan error, 1)
	go func() {
		_, err := a.AttachGroup(context.Background(), userID, targetServer.ID, "alpha", 77)
		attachDone <- err
	}()
	waitForMutationGateRefs(t, a, mutationKey{ServerID: targetServer.ID, SubID: "alpha"}, 2)
	close(target.addClientBlock)
	if err := <-copyDone; err != nil {
		t.Fatal(err)
	}
	if err := <-attachDone; err != nil {
		t.Fatal(err)
	}
	if len(target.addClients) != 1 {
		t.Fatalf("duplicate client creations=%d", len(target.addClients))
	}
}
