package keeper

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/irisnet/irishub/app/v1/params"
	"github.com/irisnet/irishub/app/v2/htlc/internal/types"
	"github.com/irisnet/irishub/codec"
	sdk "github.com/irisnet/irishub/types"
	"github.com/tendermint/tendermint/crypto"
)

type Keeper struct {
	storeKey sdk.StoreKey
	cdc      *codec.Codec
	bk       types.BankKeeper

	// codespace
	codespace sdk.CodespaceType
	// params subspace
	paramSpace params.Subspace
}

func NewKeeper(cdc *codec.Codec, key sdk.StoreKey, bk types.BankKeeper, codespace sdk.CodespaceType, paramSpace params.Subspace) Keeper {
	return Keeper{
		storeKey:   key,
		cdc:        cdc,
		bk:         bk,
		codespace:  codespace,
		paramSpace: paramSpace.WithTypeTable(types.ParamTypeTable()),
	}
}

// Codespace returns the codespace
func (k Keeper) Codespace() sdk.CodespaceType {
	return k.codespace
}

// GetCdc returns the cdc
func (k Keeper) GetCdc() *codec.Codec {
	return k.cdc
}

// CreateHTLC creates a HTLC
func (k Keeper) CreateHTLC(ctx sdk.Context, htlc types.HTLC, secretHashLock []byte) (sdk.Tags, sdk.Error) {
	// check if the secret hash lock already exists
	if k.HasSecretHashLock(ctx, secretHashLock) {
		return nil, types.ErrSecretHashLockAlreadyExists(types.DefaultCodespace, fmt.Sprintf("the secret hash lock already exists: %s", hex.EncodeToString(secretHashLock)))
	}

	// transfer the specified tokens to a dedicated HTLC Address
	htlcAddr := getHTLCAddress(htlc.OutAmount.Denom)
	if _, err := k.bk.SendCoins(ctx, htlc.Sender, htlcAddr, sdk.Coins{htlc.OutAmount}); err != nil {
		return nil, err
	}

	// add to coinflow
	ctx.CoinFlowTags().AppendCoinFlowTag(ctx, htlc.Sender.String(), htlcAddr.String(), htlc.OutAmount.String(), sdk.CoinHTLCCreateFlow, "")

	// set the htlc
	k.SetHTLC(ctx, htlc, secretHashLock)

	// add to the expiration queue
	k.AddHTLCToExpireQueue(ctx, htlc.ExpireHeight, secretHashLock)

	createTags := sdk.NewTags(
		types.TagSender, []byte(htlc.Sender.String()),
		types.TagReceiver, []byte(htlc.Receiver.String()),
		types.TagReceiverOnOtherChain, htlc.ReceiverOnOtherChain,
		types.TagSecretHashLock, []byte(hex.EncodeToString(secretHashLock)),
	)

	return createTags, nil
}

func (k Keeper) ClaimHTLC(ctx sdk.Context, secret []byte, secretHashLock []byte) (sdk.Tags, sdk.Error) {

	// get the htlc
	htlc, err := k.GetHTLC(ctx, secretHashLock)
	if err != nil {
		return nil, err
	}

	// check if not open
	if htlc.State != types.StateOpen {
		return nil, types.ErrStateIsNotOpen(k.codespace, fmt.Sprintf("HTLC state is not Open."))
	}

	// check if secret not valid
	if !bytes.Equal(k.GetSecretHashLock(secret, htlc.Timestamp), secretHashLock) {
		return nil, types.ErrInvalidSecret(k.codespace, fmt.Sprintf("invalid secret: %s", hex.EncodeToString(secret)))
	}

	// do claim
	htlcAddr := getHTLCAddress(htlc.OutAmount.Denom)
	if _, err := k.bk.SendCoins(ctx, htlcAddr, htlc.Receiver, sdk.Coins{htlc.OutAmount}); err != nil {
		return nil, err
	}

	// update secret and state in HTLC
	htlc.Secret = secret
	htlc.State = types.StateCompleted
	k.SetHTLC(ctx, htlc, secretHashLock)

	// add to coinflow
	ctx.CoinFlowTags().AppendCoinFlowTag(ctx, htlcAddr.String(), htlc.Receiver.String(), htlc.OutAmount.String(), sdk.CoinHTLCClaimFlow, "")

	calimTags := sdk.NewTags(
		types.TagSender, []byte(htlc.Sender.String()),
		types.TagReceiver, []byte(htlc.Receiver.String()),
		types.TagSecretHashLock, []byte(hex.EncodeToString(secretHashLock)),
		types.TagSecret, []byte(hex.EncodeToString(secret)),
	)

	return calimTags, nil
}

func (k Keeper) RefundHTLC(ctx sdk.Context, secretHashLock []byte) (sdk.Tags, sdk.Error) {

	// get the htlc
	htlc, err := k.GetHTLC(ctx, secretHashLock)
	if err != nil {
		return nil, err
	}

	// check if not expired
	if htlc.State != types.StateExpired {
		return nil, types.ErrStateIsNotOpen(k.codespace, fmt.Sprintf("HTLC state is not Expired."))
	}

	// do refund
	htlcAddr := getHTLCAddress(htlc.OutAmount.Denom)
	if _, err := k.bk.SendCoins(ctx, htlcAddr, htlc.Sender, sdk.Coins{htlc.OutAmount}); err != nil {
		return nil, err
	}

	// update state in HTLC
	htlc.State = types.StateRefunded
	k.SetHTLC(ctx, htlc, secretHashLock)

	// add to coinflow
	ctx.CoinFlowTags().AppendCoinFlowTag(ctx, htlcAddr.String(), htlc.Sender.String(), htlc.OutAmount.String(), sdk.CoinHTLCRefundFlow, "")

	refundTags := sdk.NewTags(
		types.TagSender, []byte(htlc.Sender.String()),
		types.TagSecretHashLock, []byte(hex.EncodeToString(secretHashLock)),
	)

	return refundTags, nil
}

// GetSecretHashLock calculates the secret hash lock
func (k Keeper) GetSecretHashLock(secret []byte, timestamp uint64) []byte {
	return sdk.SHA256(append(secret, sdk.Uint64ToBigEndian(timestamp)...))
}

func (k Keeper) HasSecretHashLock(ctx sdk.Context, secretHashLock []byte) bool {
	store := ctx.KVStore(k.storeKey)
	return store.Has(KeyHTLC(secretHashLock))
}

// SetHTLC stores the htlc
func (k Keeper) SetHTLC(ctx sdk.Context, htlc types.HTLC, secretHashLock []byte) {
	store := ctx.KVStore(k.storeKey)

	bz := k.cdc.MustMarshalBinaryLengthPrefixed(htlc)
	store.Set(KeyHTLC(secretHashLock), bz)
}

// GetHTLC retrieves the htlc by the specified secret hash lock
func (k Keeper) GetHTLC(ctx sdk.Context, secretHashLock []byte) (types.HTLC, sdk.Error) {
	store := ctx.KVStore(k.storeKey)

	bz := store.Get(KeyHTLC(secretHashLock))
	if bz == nil {
		return types.HTLC{}, types.ErrInvalidSecretHashLock(k.codespace, fmt.Sprintf("invalid secret hash lock: %s", hex.EncodeToString(secretHashLock)))
	}

	var htlc types.HTLC
	k.cdc.MustUnmarshalBinaryLengthPrefixed(bz, &htlc)

	return htlc, nil
}

// AddHTLCToExpireQueue adds the htlc to the expiration queue
func (k Keeper) AddHTLCToExpireQueue(ctx sdk.Context, expireHeight uint64, secretHashLock []byte) {
	store := ctx.KVStore(k.storeKey)

	bz := k.cdc.MustMarshalBinaryLengthPrefixed(secretHashLock)
	store.Set(KeyHTLCExpireQueue(expireHeight, secretHashLock), bz)
}

// DeleteHTLCFromExpireQueue removes the htlc from the expiration queue
func (k Keeper) DeleteHTLCFromExpireQueue(ctx sdk.Context, expireHeight uint64, secretHashLock []byte) {
	store := ctx.KVStore(k.storeKey)

	// delete the key
	store.Delete(KeyHTLCExpireQueue(expireHeight, secretHashLock))
}

// getHTLCAddress returns a dedicated address for locking tokens by the specified denom
func getHTLCAddress(denom string) sdk.AccAddress {
	return sdk.AccAddress(crypto.AddressHash([]byte(denom)))
}
