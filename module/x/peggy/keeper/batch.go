package keeper

import (
	"strconv"

	"github.com/althea-net/peggy/module/x/peggy/types"
	"github.com/cosmos/cosmos-sdk/store/prefix"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

const OutgoingTxBatchSize = 100

// BuildOutgoingTXBatch starts the following process chain:
// - find bridged denominator for given voucher type
// - select available transactions from the outgoing transaction pool sorted by fee desc
// - persist an outgoing batch object with an incrementing ID = nonce
// - emit an event
func (k Keeper) BuildOutgoingTXBatch(ctx sdk.Context, voucherDenom types.VoucherDenom, maxElements int) (*types.OutgoingTxBatch, error) {
	if maxElements == 0 {
		return nil, sdkerrors.Wrap(types.ErrInvalid, "max elements value")
	}
	bridgedDenom := k.GetCounterpartDenominator(ctx, voucherDenom)
	if bridgedDenom == nil {
		return nil, sdkerrors.Wrap(types.ErrUnknown, "bridged denominator")
	}
	selectedTx, err := k.pickUnbatchedTX(ctx, voucherDenom, *bridgedDenom, maxElements)
	if len(selectedTx) == 0 || err != nil {
		return nil, err
	}
	totalFee := selectedTx[0].BridgeFee
	for _, tx := range selectedTx[1:] {
		totalFee = totalFee.Add(tx.BridgeFee)
	}
	nextID := k.autoIncrementID(ctx, types.KeyLastOutgoingBatchID)
	nonce := types.NewUInt64Nonce(nextID)
	batch := types.OutgoingTxBatch{
		Nonce:              nonce,
		Elements:           selectedTx,
		BridgedDenominator: *bridgedDenom,
		TotalFee:           totalFee,
		Valset:             k.GetCurrentValset(ctx),
		TokenContract:      bridgedDenom.TokenContractAddress,
	}
	k.storeBatch(ctx, batch)

	batchEvent := sdk.NewEvent(
		types.EventTypeOutgoingBatch,
		sdk.NewAttribute(sdk.AttributeKeyModule, types.ModuleName),
		sdk.NewAttribute(types.AttributeKeyContract, k.GetBridgeContractAddress(ctx).String()),
		sdk.NewAttribute(types.AttributeKeyBridgeChainID, strconv.Itoa(int(k.GetBridgeChainID(ctx)))),
		sdk.NewAttribute(types.AttributeKeyOutgoingBatchID, nonce.String()),
		sdk.NewAttribute(types.AttributeKeyNonce, nonce.String()),
	)
	ctx.EventManager().EmitEvent(batchEvent)
	return &batch, nil
}

// OutgoingTxBatchExecuted is run when the Cosmos chain detects that a batch has been executed on Ethereum
// It frees all the transactions in the batch, then cancels all earlier batches
func (k Keeper) OutgoingTxBatchExecuted(ctx sdk.Context, tokenContract types.EthereumAddress, nonce types.UInt64Nonce) error {
	b := k.GetOutgoingTXBatch(ctx, tokenContract, nonce)
	if b == nil {
		return sdkerrors.Wrap(types.ErrUnknown, "nonce")
	}

	// cleanup outgoing TX pool
	for i := range b.Elements {
		k.removePoolEntry(ctx, b.Elements[i].ID)
	}

	// Iterate through remaining batches
	k.IterateOutgoingTXBatches(ctx, func(key []byte, iter_batch types.OutgoingTxBatch) bool {
		// If the iterated batches nonce is lower than the one that was just executed, cancel it
		// TODO: iterate only over batches we need to iterate over
		if iter_batch.Nonce < b.Nonce {
			k.CancelOutgoingTXBatch(ctx, tokenContract, iter_batch.Nonce)
		}
		return false
	})

	// Delete batch since it is finished
	k.deleteBatch(ctx, *b)
	return nil
}

func (k Keeper) storeBatch(ctx sdk.Context, batch types.OutgoingTxBatch) {
	store := ctx.KVStore(k.storeKey)
	key := types.GetOutgoingTxBatchKey(batch.TokenContract, batch.Nonce)
	store.Set(key, k.cdc.MustMarshalBinaryBare(batch))
}

func (k Keeper) deleteBatch(ctx sdk.Context, batch types.OutgoingTxBatch) {
	store := ctx.KVStore(k.storeKey)
	store.Delete(types.GetOutgoingTxBatchKey(batch.TokenContract, batch.Nonce))
}

// pickUnbatchedTX find TX in pool and remove from "available" second index
func (k Keeper) pickUnbatchedTX(ctx sdk.Context, denom types.VoucherDenom, bridgedDenom types.BridgedDenominator, maxElements int) ([]types.OutgoingTransferTx, error) {
	var selectedTx []types.OutgoingTransferTx
	var err error
	k.IterateOutgoingPoolByFee(ctx, denom, func(txID uint64, tx types.OutgoingTx) bool {
		txOut := types.OutgoingTransferTx{
			ID:          txID,
			Sender:      tx.Sender,
			DestAddress: tx.DestAddress,
			Amount:      bridgedDenom.ToERC20Token(tx.Amount),
			BridgeFee:   bridgedDenom.ToERC20Token(tx.BridgeFee),
		}
		selectedTx = append(selectedTx, txOut)
		err = k.removeFromUnbatchedTXIndex(ctx, tx.BridgeFee, txID)
		return err != nil || len(selectedTx) == maxElements
	})
	return selectedTx, err
}

// GetOutgoingTXBatch loads a batch object. Returns nil when not exists.
func (k Keeper) GetOutgoingTXBatch(ctx sdk.Context, tokenContract types.EthereumAddress, nonce types.UInt64Nonce) *types.OutgoingTxBatch {
	store := ctx.KVStore(k.storeKey)
	key := types.GetOutgoingTxBatchKey(tokenContract, nonce)
	bz := store.Get(key)
	if len(bz) == 0 {
		return nil
	}
	var b types.OutgoingTxBatch
	k.cdc.MustUnmarshalBinaryBare(bz, &b)
	return &b
}

// CancelOutgoingTXBatch releases all TX in the batch and deletes the batch
func (k Keeper) CancelOutgoingTXBatch(ctx sdk.Context, tokenContract types.EthereumAddress, nonce types.UInt64Nonce) error {
	batch := k.GetOutgoingTXBatch(ctx, tokenContract, nonce)
	if batch == nil {
		return types.ErrUnknown
	}
	for _, tx := range batch.Elements {
		k.prependToUnbatchedTXIndex(ctx, batch.BridgedDenominator.ToVoucherCoin(tx.BridgeFee.Amount), tx.ID)
	}

	// Delete batch since it is finished
	k.deleteBatch(ctx, *batch)

	batchEvent := sdk.NewEvent(
		types.EventTypeOutgoingBatchCanceled,
		sdk.NewAttribute(sdk.AttributeKeyModule, types.ModuleName),
		sdk.NewAttribute(types.AttributeKeyContract, k.GetBridgeContractAddress(ctx).String()),
		sdk.NewAttribute(types.AttributeKeyBridgeChainID, strconv.Itoa(int(k.GetBridgeChainID(ctx)))),
		sdk.NewAttribute(types.AttributeKeyOutgoingBatchID, nonce.String()),
		sdk.NewAttribute(types.AttributeKeyNonce, nonce.String()),
	)
	ctx.EventManager().EmitEvent(batchEvent)
	return nil
}

// IterateOutgoingTXBatches iterates through all outgoing batches in DESC order.
func (k Keeper) IterateOutgoingTXBatches(ctx sdk.Context, cb func(key []byte, batch types.OutgoingTxBatch) bool) {
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), types.OutgoingTXBatchKey)
	iter := prefixStore.ReverseIterator(nil, nil)
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		var batch types.OutgoingTxBatch
		k.cdc.MustUnmarshalBinaryBare(iter.Value(), &batch)
		// cb returns true to stop early
		if cb(iter.Key(), batch) {
			break
		}
	}
}
