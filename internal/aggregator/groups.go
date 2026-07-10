package aggregator

import (
	"sort"

	"github.com/LyschevIvan/3xui-sub-agg/internal/xui"
)

func groupClients(rows []xui.ClientSummary) map[string]ClientGroup {
	groups := make(map[string]ClientGroup)
	for _, row := range rows {
		if row.SubID == "" {
			continue
		}

		group := groups[row.SubID]
		group.SubID = row.SubID
		group.Records = append(group.Records, ClientRef{
			RecordID:   cloneInt(row.RecordID),
			Email:      row.Email,
			SubID:      row.SubID,
			Enabled:    row.Enable,
			InboundIDs: normalizeInboundIDs(row.InboundIDs),
		})
		groups[row.SubID] = group
	}

	for subID, group := range groups {
		sort.SliceStable(group.Records, func(i, j int) bool {
			return group.Records[i].Email < group.Records[j].Email
		})
		groups[subID] = group
	}
	return groups
}

func canonicalClient(rows []ClientRef) (ClientRef, bool) {
	if len(rows) == 0 {
		return ClientRef{}, false
	}

	best := -1
	for i := range rows {
		if rows[i].RecordID == nil || *rows[i].RecordID <= 0 {
			continue
		}
		if best == -1 || *rows[i].RecordID < *rows[best].RecordID ||
			(*rows[i].RecordID == *rows[best].RecordID && rows[i].Email < rows[best].Email) {
			best = i
		}
	}
	if best == -1 {
		best = 0
		for i := 1; i < len(rows); i++ {
			if rows[i].Email < rows[best].Email {
				best = i
			}
		}
	}
	return cloneClientRef(rows[best]), true
}

func cloneClientRef(row ClientRef) ClientRef {
	row.RecordID = cloneInt(row.RecordID)
	row.InboundIDs = append([]int(nil), row.InboundIDs...)
	return row
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func normalizeInboundIDs(ids []int) []int {
	if len(ids) == 0 {
		return nil
	}
	normalized := append([]int(nil), ids...)
	sort.Ints(normalized)
	write := 1
	for read := 1; read < len(normalized); read++ {
		if normalized[read] == normalized[write-1] {
			continue
		}
		normalized[write] = normalized[read]
		write++
	}
	return normalized[:write]
}
