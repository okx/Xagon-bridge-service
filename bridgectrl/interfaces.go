package bridgectrl

import (
	"context"

	"github.com/hermeznetwork/hermez-bridge/etherman"
)

// merkleTreeStore interface for the Merkle Tree
type merkleTreeStore interface {
	Get(ctx context.Context, key []byte) ([][]byte, uint, error)
	Set(ctx context.Context, key []byte, value [][]byte, depositCount uint, depth uint8) error
	ResetMT(ctx context.Context, depositCount uint) error
	GetRoot(ctx context.Context, depositCount uint, depth uint8) ([]byte, error)
}

// bridgeStorage interface for the Bridge Tree
type bridgeStorage interface {
	GetLatestExitRoot(ctx context.Context) (*etherman.GlobalExitRoot, error)
}

// BridgeServiceStorage interface for the Bridge Service.
type BridgeServiceStorage interface {
	GetClaims(ctx context.Context, destNetwork uint, limit uint, offset uint) ([]*etherman.Claim, error)
	GetDeposits(ctx context.Context, depositCount uint, origNetwork uint, limit uint) ([]*etherman.Deposit, error)
}
