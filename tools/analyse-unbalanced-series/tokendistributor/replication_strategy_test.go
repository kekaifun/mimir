package tokendistributor

import (
	"fmt"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestSimpleReplicationStrategy_GetReplicationSet(t *testing.T) {
	sortedRingTokens, ringInstanceByToken, _ := createRingTokensInstancesZones()
	simpleReplicationStrategy := newSimpleReplicationStrategy(3, nil)
	replicationSet, err := simpleReplicationStrategy.getReplicaSet(48, sortedRingTokens, ringInstanceByToken)
	if err != nil {
		errors.Wrap(err, "unable to get replication set")
	}
	require.ElementsMatch(t, replicationSet, []Instance{"instance-2", "instance-1", "instance-0"})

	replicationSet, err = simpleReplicationStrategy.getReplicaSet(956, sortedRingTokens, ringInstanceByToken)
	if err != nil {
		errors.Wrap(err, "unable to get replication set")
	}
	require.ElementsMatch(t, replicationSet, []Instance{"instance-2", "instance-1", "instance-0"})

	replicationSet, err = simpleReplicationStrategy.getReplicaSet(97, sortedRingTokens, ringInstanceByToken)
	if err != nil {
		errors.Wrap(err, "unable to get replica set")
	}
	require.ElementsMatch(t, replicationSet, []Instance{"instance-1", "instance-0", "instance-2"})

	replicationSet, err = simpleReplicationStrategy.getReplicaSet(50, sortedRingTokens, ringInstanceByToken)
	if err != nil {
		errors.Wrap(err, "unable to get replica set")
	}
	require.ElementsMatch(t, replicationSet, []Instance{"instance-1", "instance-0", "instance-2"})
}

func TestSimpleReplicationStrategy_GetReplicationStart(t *testing.T) {
	tests := map[string]struct {
		initialToken             Token
		instance                 Instance
		expectedReplicationStart Token
	}{
		"existing token preceded by the same instance": {
			initialToken:             48,
			instance:                 "instance-2",
			expectedReplicationStart: 48,
		},
		"non-existing token preceded by the same instance": {
			initialToken:             48,
			instance:                 "instance-2",
			expectedReplicationStart: 48,
		},
		"existing token preceded by different instance that does not form a full replica": {
			initialToken:             97,
			instance:                 "instance-1",
			expectedReplicationStart: 902,
		},
		"existing token preceded by different instance that form a full replica": {
			initialToken:             194,
			instance:                 "instance-0",
			expectedReplicationStart: 853,
		},
	}
	sortedRingTokens, ringInstanceByToken, _ := createRingTokensInstancesZones()
	simpleReplicationStrategy := newSimpleReplicationStrategy(3, nil)
	for _, testData := range tests {
		replicaStart, err := simpleReplicationStrategy.getReplicaStart(testData.initialToken, sortedRingTokens, ringInstanceByToken)
		if err != nil {
			errors.Wrap(err, "unable to get replica start")
		}
		require.Equal(t, replicaStart, testData.expectedReplicationStart)
	}
}

func TestSimpleReplicationStrategy_ReplicationStartAndReplicationSetConsistency(t *testing.T) {
	sortedRingTokens, ringInstanceByToken, _ := createRingTokensInstancesZones()
	simpleReplicationStrategy := newSimpleReplicationStrategy(3, nil)
	for _, token := range sortedRingTokens {
		replicaStart, err := simpleReplicationStrategy.getReplicaStart(token, sortedRingTokens, ringInstanceByToken)
		if err != nil {
			errors.Wrap(err, "unable to get replica set")
		}
		fmt.Printf("Replica start of token %d is token %d\n", token, replicaStart)
		lastReplicaToken, err := simpleReplicationStrategy.getLastReplicaToken(replicaStart, sortedRingTokens, ringInstanceByToken)
		if err != nil {
			errors.Wrap(err, "unable to get last replica token")
		}
		require.GreaterOrEqual(t, replicaStart.distance(lastReplicaToken, maxTokenValue), replicaStart.distance(token, maxTokenValue))
	}
}

func TestZoneAwareReplicationStrategy_GetReplicationSet(t *testing.T) {
	sortedRingTokens, ringInstanceByToken, zoneByInstance := createRingTokensInstancesZones()
	replicationStrategy := newZoneAwareReplicationStrategy(3, zoneByInstance, nil, nil)
	replicationSet, err := replicationStrategy.getReplicaSet(48, sortedRingTokens, ringInstanceByToken)
	if err != nil {
		errors.Wrap(err, "unable to get replication set")
	}
	require.ElementsMatch(t, replicationSet, []Instance{"instance-2", "instance-1", "instance-0"})

	replicationSet, err = replicationStrategy.getReplicaSet(50, sortedRingTokens, ringInstanceByToken)
	if err != nil {
		errors.Wrap(err, "unable to get replication set")
	}
	require.ElementsMatch(t, replicationSet, []Instance{"instance-2", "instance-1", "instance-0"})

	replicationSet, err = replicationStrategy.getReplicaSet(194, sortedRingTokens, ringInstanceByToken)
	if err != nil {
		errors.Wrap(err, "unable to get replica set")
	}
	require.ElementsMatch(t, replicationSet, []Instance{"instance-0", "instance-2", "instance-1"})

	replicationSet, err = replicationStrategy.getReplicaSet(190, sortedRingTokens, ringInstanceByToken)
	if err != nil {
		errors.Wrap(err, "unable to get replica set")
	}
	require.ElementsMatch(t, replicationSet, []Instance{"instance-0", "instance-2", "instance-1"})
}

func TestZoneAwareReplicationStrategy_ReplicationStartAndReplicationSetConsistency(t *testing.T) {
	sortedRingTokens, ringInstanceByToken, zoneByInstance := createRingTokensInstancesZones()
	replicationStrategy := newZoneAwareReplicationStrategy(3, zoneByInstance, nil, nil)
	for token, instance := range ringInstanceByToken {
		replicaStart, err := replicationStrategy.getReplicaStart(token, sortedRingTokens, ringInstanceByToken)
		if err != nil {
			errors.Wrap(err, "unable to get replica set")
		}
		fmt.Printf("Replica start of token %d (%s, %s) is token %d (%s, %s)\n", token, instance, zoneByInstance[instance], replicaStart, ringInstanceByToken[replicaStart], zoneByInstance[ringInstanceByToken[replicaStart]])
		lastReplicaToken, err := replicationStrategy.getLastReplicaToken(replicaStart, sortedRingTokens, ringInstanceByToken)
		if err != nil {
			errors.Wrap(err, "unable to get last replica token")
		}
		require.GreaterOrEqual(t, replicaStart.distance(lastReplicaToken, maxTokenValue), replicaStart.distance(token, maxTokenValue))
	}
}
