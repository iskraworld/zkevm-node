package aggregator2

import (
	"context"
	"sync"
	"testing"
	"time"

	aggrMocks "github.com/0xPolygonHermez/zkevm-node/aggregator_v2/mocks"
	pb2 "github.com/0xPolygonHermez/zkevm-node/aggregator_v2/pb"
	"github.com/0xPolygonHermez/zkevm-node/config/types"
	"github.com/0xPolygonHermez/zkevm-node/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSendFinalProof(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	cfg := Config{
		Host:                       "0.0.0.0",
		Port:                       8888,
		IntervalToConsolidateState: types.NewDuration(time.Second),
		IntervalToSendFinalProof:   types.NewDuration(30 * time.Second),
	}
	p := new(aggrMocks.ProverMock)
	st := new(aggrMocks.StateMock)
	etherMan := new(aggrMocks.EthermanMock)
	ethTxMan := new(aggrMocks.EthTxManagerMock)
	a, err := New(cfg, st, ethTxMan, etherMan)
	require.NoError(err)
	go a.Start(ctx)
	proofID := "proofID"
	prover := "prover"
	proof := &state.Proof2{
		BatchNumber:      0,
		BatchNumberFinal: 42,
		Proof:            "banana",
		InputProver:      &pb2.InputProver{},
		ProofID:          &proofID,
		Prover:           &prover,
		Aggregating:      false,
	}
	tick := time.NewTicker(time.Minute)
	wg := sync.WaitGroup{}
	wg.Add(1)

	sent, err := a.sendFinalProof(ctx, p, proof, tick)

	require.NoError(err)
	assert.False(sent)
}
