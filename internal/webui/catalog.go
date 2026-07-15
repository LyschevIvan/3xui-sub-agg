package webui

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/LyschevIvan/3xui-sub-agg/internal/aggregator"
	"github.com/LyschevIvan/3xui-sub-agg/internal/auth"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
)

type catalogSubscription struct {
	clientCard
	SubID           string
	QuerySubID      string
	Groups          []storage.ClientGroup
	ConnectionCount int
	ServerCount     int
}

type catalogInbound struct {
	ServerID          int64
	ServerName        string
	InboundID         int
	Remark            string
	Protocol          string
	Network           string
	Security          string
	SecurityClass     string
	Port              int
	Endpoint          string
	Enabled           bool
	Mutable           bool
	State             aggregator.ServerState
	StateLabel        string
	StateClass        string
	SubscriptionCount int
	SubscriptionLabel string
	SubIDs            []string
}

type catalog struct {
	Inbounds      []catalogInbound
	Subscriptions []catalogSubscription
}

func buildCatalog(
	snapshot *aggregator.Snapshot,
	userID int64,
	subBase string,
	memberships map[string][]storage.ClientGroup,
) catalog {
	if snapshot == nil {
		snapshot = &aggregator.Snapshot{}
	}

	inboundMembers := make(map[[2]int64]map[string]struct{})
	for _, server := range snapshot.Servers {
		if server.UserID != userID {
			continue
		}
		for subID, group := range server.Groups {
			if subID == "" {
				continue
			}
			for _, record := range group.Records {
				for _, inboundID := range record.InboundIDs {
					key := [2]int64{server.ID, int64(inboundID)}
					if inboundMembers[key] == nil {
						inboundMembers[key] = make(map[string]struct{})
					}
					inboundMembers[key][subID] = struct{}{}
				}
			}
		}
	}

	out := catalog{}
	for _, server := range snapshot.Servers {
		if server.UserID != userID {
			continue
		}
		stateLabel, stateClass := serverStatePresentation(server.State)
		for _, inbound := range server.Inbounds {
			key := [2]int64{server.ID, int64(inbound.ID)}
			subIDs := make([]string, 0, len(inboundMembers[key]))
			for subID := range inboundMembers[key] {
				subIDs = append(subIDs, subID)
			}
			sort.Strings(subIDs)
			security := securityLabel(inbound.Security)
			endpoint := catalogEndpoint(server.PublicHost, inbound.Port)
			out.Inbounds = append(out.Inbounds, catalogInbound{
				ServerID: server.ID, ServerName: server.Name,
				InboundID: inbound.ID, Remark: inbound.Remark,
				Protocol: strings.ToLower(inbound.Protocol),
				Network:  networkLabel(inbound.Network), Security: security,
				SecurityClass: strings.ToLower(security), Port: inbound.Port,
				Endpoint: endpoint, Enabled: inbound.Enable,
				Mutable: inbound.Enable && strings.EqualFold(inbound.Protocol, "vless") && server.State == aggregator.ServerOK,
				State:   server.State, StateLabel: stateLabel, StateClass: stateClass,
				SubscriptionCount: len(subIDs), SubscriptionLabel: subscriptionCountLabel(len(subIDs)), SubIDs: subIDs,
			})
		}
	}
	sort.Slice(out.Inbounds, func(i, j int) bool {
		left, right := out.Inbounds[i], out.Inbounds[j]
		if strings.ToLower(left.ServerName) != strings.ToLower(right.ServerName) {
			return strings.ToLower(left.ServerName) < strings.ToLower(right.ServerName)
		}
		if strings.ToLower(left.Remark) != strings.ToLower(right.Remark) {
			return strings.ToLower(left.Remark) < strings.ToLower(right.Remark)
		}
		return left.InboundID < right.InboundID
	})

	for _, card := range buildClientCards(snapshot, userID, subBase) {
		serverCount := 0
		connectionCount := 0
		for _, server := range card.Servers {
			if len(server.Inbounds) == 0 {
				continue
			}
			serverCount++
			connectionCount += len(server.Inbounds)
		}
		out.Subscriptions = append(out.Subscriptions, catalogSubscription{
			clientCard: card, SubID: card.Sub, QuerySubID: url.QueryEscape(card.Sub),
			Groups: memberships[card.Sub], ConnectionCount: connectionCount, ServerCount: serverCount,
		})
	}
	return out
}

func catalogEndpoint(publicHost string, inboundPort int) string {
	publicHost = strings.TrimSpace(publicHost)
	if publicHost == "" {
		return fmt.Sprintf(":%d", inboundPort)
	}
	if _, _, err := net.SplitHostPort(publicHost); err == nil {
		return publicHost
	}
	if strings.Count(publicHost, ":") > 1 {
		return net.JoinHostPort(strings.Trim(publicHost, "[]"), strconv.Itoa(inboundPort))
	}
	return fmt.Sprintf("%s:%d", publicHost, inboundPort)
}

func subscriptionCountLabel(count int) string {
	return russianCountLabel(count, "подписка", "подписки", "подписок")
}

func connectionCountLabel(count int) string {
	return russianCountLabel(count, "подключение", "подключения", "подключений")
}

func serverCountLabel(count int) string {
	return russianCountLabel(count, "сервер", "сервера", "серверов")
}

func russianCountLabel(count int, one, few, many string) string {
	mod100 := count % 100
	mod10 := count % 10
	suffix := many
	if mod100 < 11 || mod100 > 14 {
		switch mod10 {
		case 1:
			suffix = one
		case 2, 3, 4:
			suffix = few
		}
	}
	return fmt.Sprintf("%d %s", count, suffix)
}

func (c catalog) inbound(serverID int64, inboundID int) (catalogInbound, bool) {
	for _, inbound := range c.Inbounds {
		if inbound.ServerID == serverID && inbound.InboundID == inboundID {
			return inbound, true
		}
	}
	return catalogInbound{}, false
}

func (c catalog) subscription(subID string) (catalogSubscription, bool) {
	for _, subscription := range c.Subscriptions {
		if subscription.SubID == subID {
			return subscription, true
		}
	}
	return catalogSubscription{}, false
}

func (h *Handler) loadCatalog(r *http.Request, userID int64) (catalog, error) {
	memberships, err := h.Store.ClientGroupMemberships(userID)
	if err != nil {
		return catalog{}, err
	}
	user := authUserForCatalog(r, userID)
	return buildCatalog(h.Agg.Snapshot(), userID, h.subscriptionBase(r, user), memberships), nil
}

func authUserForCatalog(r *http.Request, userID int64) *storage.User {
	user := auth.FromContext(r.Context())
	if user != nil && user.ID == userID {
		return user
	}
	return &storage.User{ID: userID}
}
