package pools

import (
	"context"
	"fmt"

	"cosmossdk.io/math"
	storetypes "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/osmosis-labs/sqs/sqsdomain"

	"github.com/osmosis-labs/sqs/domain"

	"github.com/osmosis-labs/osmosis/osmomath"
	clmath "github.com/osmosis-labs/osmosis/v25/x/concentrated-liquidity/math"
	concentratedmodel "github.com/osmosis-labs/osmosis/v25/x/concentrated-liquidity/model"
	"github.com/osmosis-labs/osmosis/v25/x/concentrated-liquidity/swapstrategy"
	"github.com/osmosis-labs/osmosis/v25/x/poolmanager"
	poolmanagertypes "github.com/osmosis-labs/osmosis/v25/x/poolmanager/types"
)

var _ sqsdomain.RoutablePool = &routableConcentratedPoolImpl{}
var zeroDec = osmomath.ZeroDec()
var zeroBigDec = osmomath.ZeroBigDec()

type routableConcentratedPoolImpl struct {
	ChainPool     *concentratedmodel.Pool "json:\"cl_pool\""
	TickModel     *sqsdomain.TickModel    "json:\"tick_model\""
	TokenOutDenom string                  "json:\"token_out_denom\""
	TakerFee      osmomath.Dec            "json:\"taker_fee\""
}

// Size is roughly `keys * (2.5 * Key_size + 2*value_size)`. (Plus whatever excess overhead hashmaps internally have)
// key is 8 bytes, value is ~152 bytes
// so at 100k keys its max RAM of ~30MB
var tickToSqrtPriceCache, _ = lru.New2Q[int64, osmomath.BigDec](1000000)

func getTickToSqrtPrice(tick int64) (osmomath.BigDec, error) {
	if sqrtPrice, ok := tickToSqrtPriceCache.Get(tick); ok {
		return sqrtPrice, nil
	}

	sqrtPrice, err := clmath.TickToSqrtPrice(tick)
	if err != nil {
		tickToSqrtPriceCache.Add(tick, sqrtPrice)
	}
	return sqrtPrice, err
}

// GetPoolDenoms implements sqsdomain.RoutablePool.
func (r *routableConcentratedPoolImpl) GetPoolDenoms() []string {
	return r.ChainPool.GetPoolDenoms(sdk.Context{})
}

// GetType implements sqsdomain.RoutablePool.
func (r *routableConcentratedPoolImpl) GetType() poolmanagertypes.PoolType {
	return poolmanagertypes.Concentrated
}

// GetId implements sqsdomain.RoutablePool.
func (r *routableConcentratedPoolImpl) GetId() uint64 {
	return r.ChainPool.Id
}

// GetSpreadFactor implements sqsdomain.RoutablePool.
func (r *routableConcentratedPoolImpl) GetSpreadFactor() math.LegacyDec {
	return r.ChainPool.SpreadFactor
}

// GetTakerFee implements sqsdomain.RoutablePool.
func (r *routableConcentratedPoolImpl) GetTakerFee() math.LegacyDec {
	return r.TakerFee
}

// CalculateTokenOutByTokenIn implements sqsdomain.RoutablePool.
// It calculates the amount of token out given the amount of token in for a concentrated liquidity pool.
// Fails if:
// - the underlying chain pool set on the routable pool is not of concentrated type
// - fails to retrieve the tick model for the pool
// - the current tick is not within the specified current bucket range
// - tick model has no liquidity flag set
// - the current sqrt price is zero
// - rans out of ticks during swap (token in is too high for liquidity in the pool)
func (r *routableConcentratedPoolImpl) CalculateTokenOutByTokenIn(ctx context.Context, tokenIn sdk.Coin) (sdk.Coin, error) {
	concentratedPool := r.ChainPool
	tickModel := r.TickModel

	if tickModel == nil {
		return sdk.Coin{}, domain.ConcentratedPoolNoTickModelError{
			PoolId: r.ChainPool.Id,
		}
	}

	// Ensure pool has liquidity.
	if tickModel.HasNoLiquidity {
		return sdk.Coin{}, domain.ConcentratedNoLiquidityError{
			PoolId: concentratedPool.Id,
		}
	}

	// Ensure that the current bucket is within the available bucket range.
	currentBucketIndex := tickModel.CurrentTickIndex

	if currentBucketIndex < 0 || currentBucketIndex >= int64(len(tickModel.Ticks)) {
		return sdk.Coin{}, domain.ConcentratedCurrentTickNotWithinBucketError{
			PoolId:             concentratedPool.Id,
			CurrentBucketIndex: currentBucketIndex,
			TotalBuckets:       int64(len(tickModel.Ticks)),
		}
	}

	currentBucket := tickModel.Ticks[currentBucketIndex]

	isCurrentTickWithinBucket := concentratedPool.IsCurrentTickInRange(currentBucket.LowerTick, currentBucket.UpperTick)
	if !isCurrentTickWithinBucket {
		return sdk.Coin{}, domain.ConcentratedCurrentTickAndBucketMismatchError{
			PoolID:      concentratedPool.Id,
			CurrentTick: concentratedPool.CurrentTick,
			LowerTick:   currentBucket.LowerTick,
			UpperTick:   currentBucket.UpperTick,
		}
	}

	// Set the appropriate token out denom.
	isZeroForOne := tokenIn.Denom == concentratedPool.Token0
	tokenOutDenom := concentratedPool.Token0
	if isZeroForOne {
		tokenOutDenom = concentratedPool.Token1
	}

	// Initialize the swap strategy.
	swapStrategy := swapstrategy.New(isZeroForOne, zeroBigDec, &storetypes.KVStoreKey{}, concentratedPool.SpreadFactor)

	var (
		// Swap state
		currentSqrtPrice = concentratedPool.GetCurrentSqrtPrice()

		amountRemainingIn = tokenIn.Amount.ToLegacyDec()
		amountOutTotal    = osmomath.ZeroDec()
	)

	if currentSqrtPrice.IsZero() {
		return sdk.Coin{}, domain.ConcentratedZeroCurrentSqrtPriceError{
			PoolId: concentratedPool.Id,
		}
	}

	// Compute swap over all buckets.
	for amountRemainingIn.GT(zeroDec) {
		if currentBucketIndex >= int64(len(tickModel.Ticks)) || currentBucketIndex < 0 {
			// This happens when there is not enough liquidity in the pool to complete the swap
			// for a given amount of token in.
			return sdk.Coin{}, domain.ConcentratedNotEnoughLiquidityToCompleteSwapError{
				PoolId:   concentratedPool.Id,
				AmountIn: sdk.NewCoins(tokenIn).String(),
			}
		}

		currentBucket = tickModel.Ticks[currentBucketIndex]

		// Compute the next initialized tick index depending on the swap direction.
		// Zero for one - in the lower tick direction.
		// One for zero - in the upper tick direction.
		var nextInitializedTickIndex int64
		if isZeroForOne {
			nextInitializedTickIndex = currentBucket.LowerTick
			currentBucketIndex--
		} else {
			nextInitializedTickIndex = currentBucket.UpperTick
			currentBucketIndex++
		}

		// Get the sqrt price for the next initialized tick index.
		sqrtPriceTarget, err := getTickToSqrtPrice(nextInitializedTickIndex)
		if err != nil {
			return sdk.Coin{}, err
		}

		// Compute the swap within current bucket
		sqrtPriceNext, amountInConsumed, amountOutComputed, spreadRewardChargeTotal := swapStrategy.ComputeSwapWithinBucketOutGivenIn(currentSqrtPrice, sqrtPriceTarget, currentBucket.LiquidityAmount, amountRemainingIn)

		// Update swap state for next iteration
		amountRemainingIn = amountRemainingIn.SubMut(amountInConsumed).SubMut(spreadRewardChargeTotal)
		amountOutTotal = amountOutTotal.AddMut(amountOutComputed)

		// Update current sqrt price
		currentSqrtPrice = sqrtPriceNext
	}

	// Return the total amount out.
	//nolint:all
	return sdk.Coin{tokenOutDenom, amountOutTotal.TruncateInt()}, nil
}

// GetTokenOutDenom implements RoutablePool.
func (r *routableConcentratedPoolImpl) GetTokenOutDenom() string {
	return r.TokenOutDenom
}

// String implements sqsdomain.RoutablePool.
func (r *routableConcentratedPoolImpl) String() string {
	concentratedPool := r.ChainPool
	return fmt.Sprintf("pool (%d), pool type (%d), pool denoms (%v), token out (%s)", concentratedPool.Id, poolmanagertypes.Concentrated, concentratedPool.GetPoolDenoms(sdk.Context{}), r.TokenOutDenom)
}

// ChargeTakerFee implements sqsdomain.RoutablePool.
// Charges the taker fee for the given token in and returns the token in after the fee has been charged.
func (r *routableConcentratedPoolImpl) ChargeTakerFeeExactIn(tokenIn sdk.Coin) (tokenInAfterFee sdk.Coin) {
	tokenInAfterTakerFee, _ := poolmanager.CalcTakerFeeExactIn(tokenIn, r.GetTakerFee())
	return tokenInAfterTakerFee
}

// SetTokenOutDenom implements sqsdomain.RoutablePool.
func (r *routableConcentratedPoolImpl) SetTokenOutDenom(tokenOutDenom string) {
	r.TokenOutDenom = tokenOutDenom
}

// CalcSpotPrice implements sqsdomain.RoutablePool.
func (r *routableConcentratedPoolImpl) CalcSpotPrice(ctx context.Context, baseDenom string, quoteDenom string) (osmomath.BigDec, error) {
	spotPrice, err := r.ChainPool.SpotPrice(sdk.Context{}, quoteDenom, baseDenom)
	if err != nil {
		return osmomath.BigDec{}, err
	}
	return spotPrice, nil
}

// IsGeneralizedCosmWasmPool implements sqsdomain.RoutablePool.
func (*routableConcentratedPoolImpl) IsGeneralizedCosmWasmPool() bool {
	return false
}

// GetCodeID implements sqsdomain.RoutablePool.
func (r *routableConcentratedPoolImpl) GetCodeID() uint64 {
	return notCosmWasmPoolCodeID
}
