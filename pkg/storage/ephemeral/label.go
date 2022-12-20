// SPDX-License-Identifier: AGPL-3.0-only

package ephemeral

import (
	"fmt"

	"github.com/prometheus/prometheus/model/labels"
)

const (
	// EphemeralLabelName is used to ask for ephemeral data in ingesters.
	EphemeralLabelName = "__ephemeral__"
)

// IsEphemeralQuery extracts a ShardSelector and the index it was pulled from the matcher list.
func IsEphemeralQuery(matchers []*labels.Matcher) (bool, int, error) {
	for idx, matcher := range matchers {
		if matcher.Name == EphemeralLabelName && matcher.Type == labels.MatchEqual {
			switch matcher.Value {
			case "true":
				return true, idx, nil
			case "false":
				return false, idx, nil
			default:
				return false, idx, fmt.Errorf("invalid ephemeral label")
			}
		}
	}
	return false, -1, nil
}

// RemoveEphemeralMatcher returns the input matchers without the label matcher on the query shard (if any).
func RemoveEphemeralMatcher(matchers []*labels.Matcher) (ephemeral bool, filtered []*labels.Matcher, err error) {
	ephemeral, idx, err := IsEphemeralQuery(matchers)
	if err != nil || idx < 0 {
		return false, matchers, err
	}

	// Create a new slice with the shard matcher removed.
	filtered = make([]*labels.Matcher, 0, len(matchers)-1)
	filtered = append(filtered, matchers[:idx]...)
	filtered = append(filtered, matchers[idx+1:]...)

	return ephemeral, filtered, nil
}