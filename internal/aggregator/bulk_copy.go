package aggregator

import (
	"context"
	"sort"
)

type BulkCopyStatus string

const (
	BulkCopyAdded           BulkCopyStatus = "added"
	BulkCopyAlreadyAttached BulkCopyStatus = "already_attached"
	BulkCopyFailed          BulkCopyStatus = "failed"
)

type BulkCopyItem struct {
	SubID  string
	Status BulkCopyStatus
}

type BulkCopyResult struct {
	Items                          []BulkCopyItem
	Added, AlreadyAttached, Failed int
}

func normalizeBulkSubIDs(subIDs []string) ([]string, error) {
	unique := make(map[string]struct{}, len(subIDs))
	for _, subID := range subIDs {
		if subID == "" {
			continue
		}
		unique[subID] = struct{}{}
	}
	if len(unique) == 0 || len(unique) > 500 {
		return nil, errInvalidMutation
	}
	out := make([]string, 0, len(unique))
	for subID := range unique {
		out = append(out, subID)
	}
	sort.Strings(out)
	return out, nil
}

// CopyGroupsToInbound safely adds logical users to one target inbound. It is
// intentionally non-destructive: no source detach or delete operation exists
// in this code path, and every item can be retried idempotently.
func (a *Aggregator) CopyGroupsToInbound(
	ctx context.Context,
	userID, targetServerID int64,
	targetInboundID int,
	subIDs []string,
) (BulkCopyResult, error) {
	normalized, err := normalizeBulkSubIDs(subIDs)
	if err != nil || userID <= 0 || targetServerID <= 0 || targetInboundID <= 0 {
		return BulkCopyResult{}, errInvalidMutation
	}
	target, err := a.ownedMutationClient(userID, targetServerID)
	if err != nil {
		return BulkCopyResult{}, err
	}
	document, err := a.freshInboundDocument(ctx, target, targetInboundID)
	if err != nil {
		return BulkCopyResult{}, err
	}
	if err := requireVLESS(document); err != nil {
		return BulkCopyResult{}, err
	}

	result := BulkCopyResult{Items: make([]BulkCopyItem, 0, len(normalized))}
	for _, subID := range normalized {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		mutation, mutationErr := a.AttachGroup(ctx, userID, targetServerID, subID, targetInboundID)
		item := BulkCopyItem{SubID: subID}
		switch {
		case mutationErr != nil:
			item.Status = BulkCopyFailed
			result.Failed++
		case mutation.Noop:
			item.Status = BulkCopyAlreadyAttached
			result.AlreadyAttached++
		default:
			item.Status = BulkCopyAdded
			result.Added++
		}
		result.Items = append(result.Items, item)
	}
	return result, nil
}
