package aggregator

import (
	"reflect"
	"slices"
	"testing"

	"github.com/LyschevIvan/3xui-sub-agg/internal/xui"
)

func TestGroupClientsPreservesDuplicateRecords(t *testing.T) {
	recordID := 7
	rows := []xui.ClientSummary{
		{RecordID: &recordID, Email: "z@example", SubID: "same", Enable: true, InboundIDs: []int{2, 1, 1}},
		{Email: "a@example", SubID: "same", Enable: false, InboundIDs: []int{1}},
		{Email: "orphan@example", SubID: "same", Enable: true},
	}

	groups := groupClients(rows)
	g := groups["same"]
	if len(g.Records) != 3 {
		t.Fatalf("records=%+v", g.Records)
	}
	if !slices.Equal(g.Records[0].InboundIDs, []int{1}) {
		t.Fatalf("ids=%v", g.Records[0].InboundIDs)
	}
	if len(g.Records[1].InboundIDs) != 0 {
		t.Fatalf("orphan ids=%v", g.Records[1].InboundIDs)
	}
	if !slices.Equal(g.Records[2].InboundIDs, []int{1, 2}) {
		t.Fatalf("ids=%v", g.Records[2].InboundIDs)
	}
	if g.Records[0].Enabled || !g.Records[1].Enabled || !g.Records[2].Enabled {
		t.Fatalf("enabled flags=%+v", g.Records)
	}

	// Published group state must not alias pointer or slice storage returned by
	// the panel client.
	recordID = 99
	rows[0].InboundIDs[0] = 99
	if g.Records[2].RecordID == nil || *g.Records[2].RecordID != 7 {
		t.Fatalf("record id aliased source: %+v", g.Records[2].RecordID)
	}
	if !slices.Equal(g.Records[2].InboundIDs, []int{1, 2}) {
		t.Fatalf("inbound ids aliased source: %v", g.Records[2].InboundIDs)
	}
}

func TestGroupClientsUsesExactNonEmptySubID(t *testing.T) {
	groups := groupClients([]xui.ClientSummary{
		{Email: "exact@example", SubID: "sub"},
		{Email: "spaced@example", SubID: " sub "},
		{Email: "blank@example", SubID: ""},
	})

	if len(groups) != 2 {
		t.Fatalf("groups=%+v", groups)
	}
	if groups["sub"].SubID != "sub" || groups[" sub "].SubID != " sub " {
		t.Fatalf("subIds were not grouped exactly: %+v", groups)
	}
	if _, ok := groups[""]; ok {
		t.Fatalf("empty subId group was retained: %+v", groups[""])
	}
}

func TestGroupClientsOrdersEqualEmailsDeterministically(t *testing.T) {
	id1, id2 := 1, 2
	rows := []xui.ClientSummary{
		{RecordID: &id2, Email: "same@example", SubID: "same", Enable: false, InboundIDs: []int{4}},
		{Email: "same@example", SubID: "same", Enable: false, InboundIDs: []int{1}},
		{RecordID: &id1, Email: "same@example", SubID: "same", Enable: true, InboundIDs: []int{3}},
		{RecordID: &id1, Email: "same@example", SubID: "same", Enable: true, InboundIDs: []int{2, 1, 1}},
		{RecordID: &id1, Email: "same@example", SubID: "same", Enable: false, InboundIDs: []int{2}},
	}
	expected := []ClientRef{
		{RecordID: &id1, Email: "same@example", SubID: "same", Enabled: false, InboundIDs: []int{2}},
		{RecordID: &id1, Email: "same@example", SubID: "same", Enabled: true, InboundIDs: []int{1, 2}},
		{RecordID: &id1, Email: "same@example", SubID: "same", Enabled: true, InboundIDs: []int{3}},
		{RecordID: &id2, Email: "same@example", SubID: "same", Enabled: false, InboundIDs: []int{4}},
		{Email: "same@example", SubID: "same", Enabled: false, InboundIDs: []int{1}},
	}

	permutations := 0
	var checkPermutations func(int)
	checkPermutations = func(position int) {
		if position == len(rows) {
			permutations++
			got := groupClients(rows)["same"].Records
			if !reflect.DeepEqual(got, expected) {
				t.Fatalf("permutation %d produced records=%+v; want=%+v", permutations, got, expected)
			}
			return
		}
		for i := position; i < len(rows); i++ {
			rows[position], rows[i] = rows[i], rows[position]
			checkPermutations(position + 1)
			rows[position], rows[i] = rows[i], rows[position]
		}
	}
	checkPermutations(0)
	if permutations != 120 {
		t.Fatalf("permutations=%d", permutations)
	}
}

func TestCanonicalClientUsesIDThenEmail(t *testing.T) {
	id0, id2, id7, negative := 0, 2, 7, -1
	for _, tc := range []struct {
		name  string
		rows  []ClientRef
		email string
	}{
		{"lowest id", []ClientRef{{RecordID: &id7, Email: "a"}, {RecordID: &id2, Email: "z"}}, "z"},
		{"email fallback", []ClientRef{{Email: "z"}, {Email: "a"}}, "a"},
		{"id tie", []ClientRef{{RecordID: &id2, Email: "z"}, {RecordID: &id2, Email: "a"}}, "a"},
		{"non-positive ids use email fallback", []ClientRef{{RecordID: &id0, Email: "z"}, {RecordID: &negative, Email: "a"}}, "a"},
		{"positive id beats lexicographic fallback", []ClientRef{{Email: "a"}, {RecordID: &id7, Email: "z"}}, "z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := canonicalClient(tc.rows)
			if !ok || got.Email != tc.email {
				t.Fatalf("got=%+v ok=%v", got, ok)
			}
		})
	}

	if _, ok := canonicalClient(nil); ok {
		t.Fatal("empty records unexpectedly produced a canonical client")
	}
}
