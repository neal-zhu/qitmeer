package mining

import (
	"fmt"
	"github.com/Qitmeer/qitmeer/common/hash"
	"github.com/Qitmeer/qitmeer/core/blockchain"
	"github.com/Qitmeer/qitmeer/core/blockdag"
	"github.com/Qitmeer/qitmeer/core/merkle"
	s "github.com/Qitmeer/qitmeer/core/serialization"
	"github.com/Qitmeer/qitmeer/core/types"
	"github.com/Qitmeer/qitmeer/core/types/pow"
	"github.com/Qitmeer/qitmeer/engine/txscript"
	"github.com/Qitmeer/qitmeer/log"
	"github.com/Qitmeer/qitmeer/params"
	"github.com/Qitmeer/qitmeer/services/blkmgr"
	"github.com/Qitmeer/qitmeer/services/mempool"
)

// NewBlockTemplate returns a new block template that is ready to be solved
// using the transactions from the passed transaction source pool and a coinbase
// that either pays to the passed address if it is not nil, or a coinbase that
// is redeemable by anyone if the passed address is nil.  The nil address
// functionality is useful since there are cases such as the getblocktemplate
// RPC where external mining software is responsible for creating their own
// coinbase which will replace the one generated for the block template.  Thus
// the need to have configured address can be avoided.
//
// The transactions selected and included are prioritized according to several
// factors.  First, each transaction has a priority calculated based on its
// value, age of inputs, and size.  Transactions which consist of larger
// amounts, older inputs, and small sizes have the highest priority.  Second, a
// fee per kilobyte is calculated for each transaction.  Transactions with a
// higher fee per kilobyte are preferred.  Finally, the block generation related
// policy settings are all taken into account.
//
// Transactions which only spend outputs from other transactions already in the
// block chain are immediately added to a priority queue which either
// prioritizes based on the priority (then fee per kilobyte) or the fee per
// kilobyte (then priority) depending on whether or not the BlockPrioritySize
// policy setting allots space for high-priority transactions.  Transactions
// which spend outputs from other transactions in the source pool are added to a
// dependency map so they can be added to the priority queue once the
// transactions they depend on have been included.
//
// Once the high-priority area (if configured) has been filled with
// transactions, or the priority falls below what is considered high-priority,
// the priority queue is updated to prioritize by fees per kilobyte (then
// priority).
//
// When the fees per kilobyte drop below the TxMinFreeFee policy setting, the
// transaction will be skipped unless the BlockMinSize policy setting is
// nonzero, in which case the block will be filled with the low-fee/free
// transactions until the block size reaches that minimum size.
//
// Any transactions which would cause the block to exceed the BlockMaxSize
// policy setting, exceed the maximum allowed signature operations per block, or
// otherwise cause the block to be invalid are skipped.
//
// Given the above, a block generated by this function is of the following form:
//
//   -----------------------------------  --  --
//  |      Coinbase Transaction         |   |   |
//  |-----------------------------------|   |   |
//  |                                   |   |   | ----- policy.BlockPrioritySize
//  |   High-priority Transactions      |   |   |
//  |                                   |   |   |
//  |-----------------------------------|   | --
//  |                                   |   |
//  |                                   |   |
//  |                                   |   |--- (policy.BlockMaxSize) / 2
//  |  Transactions prioritized by fee  |   |
//  |  until <= policy.TxMinFreeFee     |   |
//  |                                   |   |
//  |                                   |   |
//  |                                   |   |
//  |-----------------------------------|   |
//  |  Low-fee/Non high-priority (free) |   |
//  |  transactions (while block size   |   |
//  |  <= policy.BlockMinSize)          |   |
//   -----------------------------------  --
//
//  This function returns nil, nil if there are not enough voters on any of
//  the current top blocks to create a new block template.
// TODO, refactor NewBlockTemplate input dependencies

func NewBlockTemplate(policy *Policy, params *params.Params,
	sigCache *txscript.SigCache, txSource TxSource, timeSource blockchain.MedianTimeSource,
	blockManager *blkmgr.BlockManager, payToAddress types.Address, parents []*hash.Hash) (*types.BlockTemplate, error) {
	subsidyCache := blockManager.GetChain().FetchSubsidyCache()

	best := blockManager.GetChain().BestSnapshot()
	nextBlockHeight := uint64(0)
	nextBlockOrder := uint64(best.GraphState.GetTotal())
	//nextBlockLayer:=uint64(best.GraphState.GetLayer()+1)

	// All transaction scripts are verified using the more strict standarad
	// flags.
	scriptFlags, err := policy.StandardVerifyFlags()
	if err != nil {
		return nil, err
	}

	// Add a random coinbase nonce to ensure that tx prefix hash
	// so that our merkle root is unique for lookups needed for
	// getwork, etc.
	extraNonce, err := s.RandomUint64()
	if err != nil {
		return nil, err
	}

	parentsSet := blockdag.NewHashSet()
	if parents == nil {
		parents = blockManager.GetChain().GetMiningTips()
		parentsSet.AddList(parents)
		nextBlockHeight = uint64(blockManager.GetChain().BlockDAG().GetMainChainTip().GetHeight() + 1)
	} else {
		parentsSet.AddList(parents)
		mainp := blockManager.GetChain().BlockDAG().GetMainParent(blockManager.GetChain().BlockDAG().GetIdSet(parents))
		nextBlockHeight = uint64(mainp.GetHeight() + 1)
	}

	coinbaseScript, err := standardCoinbaseScript(nextBlockHeight, extraNonce)
	if err != nil {
		return nil, err
	}
	opReturnPkScript, err := standardCoinbaseOpReturn([]byte{})
	if err != nil {
		return nil, err
	}

	blues := int64(blockManager.GetChain().BlockDAG().GetBlues(blockManager.GetChain().BlockDAG().GetIdSet(parents)))
	coinbaseTx, err := createCoinbaseTx(subsidyCache,
		coinbaseScript,
		opReturnPkScript,
		blues,
		payToAddress,
		params)
	if err != nil {
		return nil, err
	}

	coinbaseSigOpCost := int64(blockchain.CountSigOps(coinbaseTx))
	// Get the current source transactions and create a priority queue to
	// hold the transactions which are ready for inclusion into a block
	// along with some priority related and fee metadata.  Reserve the same
	// number of items that are available for the priority queue.  Also,
	// choose the initial sort order for the priority queue based on whether
	// or not there is an area allocated for high-priority transactions.
	sourceTxns := txSource.MiningDescs()
	sortedByFee := policy.BlockPrioritySize == 0
	weightedRandQueue := newWeightedRandQueue(len(sourceTxns))
	// Create a slice to hold the transactions to be included in the
	// generated block with reserved space.  Also create a utxo view to
	// house all of the input transactions so multiple lookups can be
	// avoided.
	blockTxns := make([]*types.Tx, 0, len(sourceTxns))
	blockTxns = append(blockTxns, coinbaseTx)
	blockUtxos := blockchain.NewUtxoViewpoint()
	blockUtxos.SetViewpoints(parents)
	// dependers is used to track transactions which depend on another
	// transaction in the source pool.  This, in conjunction with the
	// dependsOn map kept with each dependent transaction helps quickly
	// determine which dependent transactions are now eligible for inclusion
	// in the block once each transaction has been included.
	dependers := make(map[hash.Hash]map[hash.Hash]*WeightedRandTx)
	// Create slices to hold the fees and number of signature operations
	// for each of the selected transactions and add an entry for the
	// coinbase.  This allows the code below to simply append details about
	// a transaction as it is selected for inclusion in the final block.
	// However, since the total fees aren't known yet, use a dummy value for
	// the coinbase fee which will be updated later.
	txFees := make([]int64, 0, len(sourceTxns))
	txSigOpCosts := make([]int64, 0, len(sourceTxns))
	txFees = append(txFees, -1) // Updated once known
	txSigOpCosts = append(txSigOpCosts, coinbaseSigOpCost)

	log.Debug("Inclusion to new block", "transactions", len(sourceTxns))
mempoolLoop:
	for _, txDesc := range sourceTxns {
		// A block can't have more than one coinbase or contain
		// non-finalized transactions.
		tx := txDesc.Tx
		if tx.Tx.IsCoinBase() {
			log.Trace(fmt.Sprintf("Skipping coinbase tx %s", tx.Hash()))
			continue
		}
		if !blockchain.IsFinalizedTransaction(tx, nextBlockHeight,
			timeSource.AdjustedTime()) {

			log.Trace(fmt.Sprintf("Skipping non-finalized tx %s", tx.Hash()))
			continue
		}

		// Fetch all of the utxos referenced by the this transaction.
		// NOTE: This intentionally does not fetch inputs from the
		// mempool since a transaction which depends on other
		// transactions in the mempool must come after those
		// dependencies in the final generated block.
		utxos, err := blockManager.GetChain().FetchUtxoView(tx)
		if err != nil {
			log.Warn(fmt.Sprintf("Unable to fetch utxo view for tx %s: %v",
				tx.Hash(), err))
			continue
		}

		// Setup dependencies for any transactions which reference
		// other transactions in the mempool so they can be properly
		// ordered below.
		weirandItem := &WeightedRandTx{tx: tx}
		for _, txIn := range tx.Tx.TxIn {
			originHash := &txIn.PreviousOut.Hash
			entry := utxos.LookupEntry(txIn.PreviousOut)
			if entry == nil || entry.IsSpent() {
				if !txSource.HaveTransaction(originHash) {
					log.Trace(fmt.Sprintf("Skipping tx %s because it "+
						"references unspent output %v "+
						"which is not available",
						tx.Hash(), txIn.PreviousOut))
					continue mempoolLoop
				}

				// The transaction is referencing another
				// transaction in the source pool, so setup an
				// ordering dependency.
				deps, exists := dependers[*originHash]
				if !exists {
					deps = make(map[hash.Hash]*WeightedRandTx)
					dependers[*originHash] = deps
				}
				deps[*weirandItem.tx.Hash()] = weirandItem
				if weirandItem.dependsOn == nil {
					weirandItem.dependsOn = make(
						map[hash.Hash]struct{})
				}
				weirandItem.dependsOn[*originHash] = struct{}{}

				// Skip the check below. We already know the
				// referenced transaction is available.
				continue
			}
		}

		// Calculate the final transaction priority using the input
		// value age sum as well as the adjusted transaction size.  The
		// formula is: sum(inputValue * inputAge) / adjustedTxSize
		weirandItem.priority = mempool.CalcPriority(tx.Tx, utxos,
			nextBlockHeight, blockManager.GetChain().BlockDAG())

		// Calculate the fee in Satoshi/kB.
		weirandItem.feePerKB = txDesc.FeePerKB
		weirandItem.fee = txDesc.Fee

		// Add the transaction to the priority queue to mark it ready
		// for inclusion in the block unless it has dependencies.
		if weirandItem.dependsOn == nil {
			weightedRandQueue.Push(weirandItem)
		}

		// Merge the referenced outputs from the input transactions to
		// this transaction into the block utxo view.  This allows the
		// code below to avoid a second lookup.
		mergeUtxoView(blockUtxos, utxos)
	}

	log.Trace(fmt.Sprintf("Weighted random queue len %d, dependers len %d",
		weightedRandQueue.Len(), len(dependers)))

	blockSize := uint32(blockHeaderOverhead) + uint32(coinbaseTx.Transaction().SerializeSize())

	blockSigOpCost := coinbaseSigOpCost
	totalFees := int64(0)

	// Choose which transactions make it into the block.
	for weightedRandQueue.Len() > 0 {
		// Grab the highest priority (or highest fee per kilobyte
		// depending on the sort order) transaction.
		weirandItem := weightedRandQueue.Pop()
		tx := weirandItem.tx

		// Grab any transactions which depend on this one.
		deps := dependers[*tx.Hash()]

		// Enforce maximum block size.  Also check for overflow.
		txSize := uint32(tx.Transaction().SerializeSize())
		blockPlusTxSize := blockSize + txSize
		if blockPlusTxSize < blockSize || blockPlusTxSize >= policy.BlockMaxSize {
			log.Trace(fmt.Sprintf("Skipping tx %s (size %v) because it "+
				"would exceed the max block size; cur block "+
				"size %v, cur num tx %v", tx.Hash(), txSize,
				blockSize, len(blockTxns)))
			logSkippedDeps(tx, deps)
			continue
		}

		// Enforce maximum signature operation cost per block.  Also
		// check for overflow.
		sigOpCost := blockchain.CountSigOps(tx)
		if blockSigOpCost+int64(sigOpCost) < blockSigOpCost ||
			blockSigOpCost+int64(sigOpCost) > blockchain.MaxSigOpsPerBlock {
			log.Trace(fmt.Sprintf("Skipping tx %s because it would "+
				"exceed the maximum sigops per block", tx.Hash()))
			logSkippedDeps(tx, deps)
			continue
		}

		// Skip free transactions once the block is larger than the
		// minimum block size.
		if sortedByFee &&
			weirandItem.feePerKB < int64(policy.TxMinFreeFee) &&
			(blockPlusTxSize >= policy.BlockMinSize) {
			log.Trace(fmt.Sprintf("Skipping tx %s with feePerKB %.2d "+
				"< TxMinFreeFee %d and block size %d >= "+
				"minBlockSize %d", tx.Hash(), weirandItem.feePerKB,
				policy.TxMinFreeFee, blockPlusTxSize,
				policy.BlockMinSize))
			logSkippedDeps(tx, deps)
			continue
		}

		// Ensure the transaction inputs pass all of the necessary
		// preconditions before allowing it to be added to the block.
		_, err = blockchain.CheckTransactionInputs(tx, blockUtxos, params, blockManager.GetChain())
		if err != nil {
			log.Trace(fmt.Sprintf("Skipping tx %s due to error in "+
				"CheckTransactionInputs: %v", tx.Hash(), err))
			logSkippedDeps(tx, deps)
			continue
		}
		err = blockchain.ValidateTransactionScripts(tx, blockUtxos,
			scriptFlags, sigCache)
		if err != nil {
			log.Trace(fmt.Sprintf("Skipping tx %s due to error in "+
				"ValidateTransactionScripts: %v", tx.Hash(), err))
			logSkippedDeps(tx, deps)
			continue
		}

		// Spend the transaction inputs in the block utxo view and add
		// an entry for it to ensure any transactions which reference
		// this one have it available as an input and can ensure they
		// aren't double spending.
		err = spendTransaction(blockUtxos, tx, &hash.ZeroHash)
		if err != nil {
			log.Warn(fmt.Sprintf("Unable to spend transaction %v in the preliminary "+
				"UTXO view for the block template: %v",
				tx.Hash(), err))
		}
		// Add the transaction to the block, increment counters, and
		// save the fees and signature operation counts to the block
		// template.
		blockTxns = append(blockTxns, tx)
		blockSize += txSize
		blockSigOpCost += int64(sigOpCost)
		totalFees += weirandItem.fee
		txFees = append(txFees, weirandItem.fee)
		txSigOpCosts = append(txSigOpCosts, int64(sigOpCost))

		log.Trace(fmt.Sprintf("Adding tx %s (priority %.2f, feePerKB %.2d)",
			weirandItem.tx.Hash(), weirandItem.priority, weirandItem.feePerKB))

		// Add transactions which depend on this one (and also do not
		// have any other unsatisified dependencies) to the priority
		// queue.
		for _, item := range deps {
			// Add the transaction to the priority queue if there
			// are no more dependencies after this one.
			delete(item.dependsOn, *tx.Hash())
			if len(item.dependsOn) == 0 {
				weightedRandQueue.Push(item)
			}
		}
	}

	//coinbaseTx.Tx.TxOut[0].Amount += uint64(totalFees)
	txFees[0] = -totalFees

	// Fill witness
	err = fillWitnessToCoinBase(blockTxns)
	if err != nil {
		return nil, miningRuleError(ErrCreatingCoinbase, err.Error())
	}

	ts := MedianAdjustedTime(blockManager.GetChain(), timeSource)

	//
	reqBlake2bDDifficulty, err := blockManager.GetChain().CalcNextRequiredDifficulty(ts, pow.BLAKE2BD)
	if err != nil {
		return nil, miningRuleError(ErrGettingDifficulty, err.Error())
	}

	//
	reqX16rv3Difficulty, err := blockManager.GetChain().CalcNextRequiredDifficulty(ts, pow.X16RV3)
	if err != nil {
		return nil, miningRuleError(ErrGettingDifficulty, err.Error())
	}

	//
	reqX8r16Difficulty, err := blockManager.GetChain().CalcNextRequiredDifficulty(ts, pow.X8R16)
	if err != nil {
		return nil, miningRuleError(ErrGettingDifficulty, err.Error())
	}

	//
	keccak256Difficulty, err := blockManager.GetChain().CalcNextRequiredDifficulty(ts, pow.QITMEERKECCAK256)
	if err != nil {
		return nil, miningRuleError(ErrGettingDifficulty, err.Error())
	}
	reqCuckarooDifficulty, err := blockManager.GetChain().CalcNextRequiredDifficulty(ts, pow.CUCKAROO)
	if err != nil {
		return nil, miningRuleError(ErrGettingDifficulty, err.Error())
	}
	reqCuckaroomDifficulty, err := blockManager.GetChain().CalcNextRequiredDifficulty(ts, pow.CUCKAROOM)
	if err != nil {
		return nil, miningRuleError(ErrGettingDifficulty, err.Error())
	}
	reqCuckatooDifficulty, err := blockManager.GetChain().CalcNextRequiredDifficulty(ts, pow.CUCKATOO)

	if err != nil {
		return nil, miningRuleError(ErrGettingDifficulty, err.Error())
	}

	// Choose the block version to generate based on the network.
	blockVersion := BlockVersion(params.Net)

	// Create a new block ready to be solved.
	merkles := merkle.BuildMerkleTreeStore(blockTxns, false)

	paMerkles := merkle.BuildParentsMerkleTreeStore(parents)
	var block types.Block
	block.Header = types.BlockHeader{
		Version:    blockVersion,
		ParentRoot: *paMerkles[len(paMerkles)-1],
		TxRoot:     *merkles[len(merkles)-1],
		StateRoot:  hash.Hash{}, //TODO, state root
		Timestamp:  ts,
		Difficulty: reqCuckaroomDifficulty,
		Pow:        pow.GetInstance(pow.CUCKAROOM, 0, []byte{}),
		// Size declared below
	}
	for _, pb := range parents {
		if err := block.AddParent(pb); err != nil {
			return nil, err
		}
	}
	for _, tx := range blockTxns {
		if err := block.AddTransaction(tx.Transaction()); err != nil {
			return nil, miningRuleError(ErrTransactionAppend, err.Error())
		}
	}

	sblock := types.NewBlock(&block)
	sblock.SetOrder(nextBlockOrder)
	sblock.SetHeight(uint(nextBlockHeight))
	err = blockManager.GetChain().CheckConnectBlockTemplate(sblock)
	if err != nil {
		str := fmt.Sprintf("failed to do final check for check connect "+
			"block when making new block template: %v",
			err.Error())
		return nil, miningRuleError(ErrCheckConnectBlock, str)
	}

	log.Debug("Created new block template",
		"transactions", len(block.Transactions),
		"expect fees", totalFees,
		"signOp", blockSigOpCost,
		"bytes", blockSize,
		"target",
		fmt.Sprintf("%064x", pow.CompactToBig(block.Header.Difficulty)))

	blockTemplate := &types.BlockTemplate{
		Block:           &block,
		Fees:            txFees,
		SigOpCounts:     txSigOpCosts,
		Height:          nextBlockHeight,
		Blues:           blues,
		ValidPayAddress: payToAddress != nil,
		PowDiffData: types.PowDiffStandard{
			Blake2bDTarget:         reqBlake2bDDifficulty,
			X16rv3DTarget:          reqX16rv3Difficulty,
			X8r16DTarget:           reqX8r16Difficulty,
			QitmeerKeccak256Target: keccak256Difficulty,
			CuckarooBaseDiff:       pow.CompactToBig(reqCuckarooDifficulty).Uint64(),
			CuckaroomBaseDiff:      pow.CompactToBig(reqCuckaroomDifficulty).Uint64(),
			CuckatooBaseDiff:       pow.CompactToBig(reqCuckatooDifficulty).Uint64(),
		},
	}
	return handleCreatedBlockTemplate(blockTemplate, blockManager)
}

// UpdateBlockTime updates the timestamp in the header of the passed block to
// the current time while taking into account the median time of the last
// several blocks to ensure the new time is after that time per the chain
// consensus rules.  Finally, it will update the target difficulty if needed
// based on the new time for the test networks since their target difficulty can
// change based upon time.
func UpdateBlockTime(msgBlock *types.Block, chain *blockchain.BlockChain, timeSource blockchain.MedianTimeSource,
	activeNetParams *params.Params) error {

	// The new timestamp is potentially adjusted to ensure it comes after
	// the median time of the last several blocks per the chain consensus
	// rules.
	newTimestamp := MedianAdjustedTime(chain, timeSource)
	msgBlock.Header.Timestamp = newTimestamp

	// If running on a network that requires recalculating the difficulty,
	// do so now.
	if activeNetParams.ReduceMinDifficulty {
		difficulty, err := chain.CalcNextRequiredDifficulty(
			newTimestamp, msgBlock.Header.Pow.GetPowType())
		if err != nil {
			return miningRuleError(ErrGettingDifficulty, err.Error())
		}
		msgBlock.Header.Difficulty = difficulty
	}

	return nil
}

// mergeUtxoView adds all of the entries in view to viewA.  The result is that
// viewA will contain all of its original entries plus all of the entries
// in viewB.  It will replace any entries in viewB which also exist in viewA
// if the entry in viewA is fully spent.
func mergeUtxoView(viewA *blockchain.UtxoViewpoint, viewB *blockchain.UtxoViewpoint) {
	viewAEntries := viewA.Entries()
	for outpoint, entryB := range viewB.Entries() {
		if entryA, exists := viewAEntries[outpoint]; !exists ||
			entryA == nil || entryA.IsSpent() {

			viewAEntries[outpoint] = entryB
		}
	}
}

// TODO, move the log logic
// logSkippedDeps logs any dependencies which are also skipped as a result of
// skipping a transaction while generating a block template at the trace level.
func logSkippedDeps(tx *types.Tx, deps map[hash.Hash]*WeightedRandTx) {
	if deps == nil {
		return
	}

	for _, item := range deps {
		log.Trace(fmt.Sprintf("Skipping tx %s since it depends on %s\n",
			item.tx.Hash(), tx.Hash()))
	}
}

// spendTransaction updates the passed view by marking the inputs to the passed
// transaction as spent.  It also adds all outputs in the passed transaction
// which are not provably unspendable as available unspent transaction outputs.
func spendTransaction(utxoView *blockchain.UtxoViewpoint, tx *types.Tx, blockHash *hash.Hash) error {
	for _, txIn := range tx.Transaction().TxIn {
		entry := utxoView.LookupEntry(txIn.PreviousOut)
		if entry != nil {
			entry.Spend()
		}

	}

	utxoView.AddTxOuts(tx, blockHash)
	return nil
}

// txIndexFromTxList returns a transaction's index in a list, or -1 if it
// can not be found.
func txIndexFromTxList(hash hash.Hash, list []*types.Tx) int {
	for i, tx := range list {
		h := tx.Hash()
		if hash == *h {
			return i
		}
	}

	return -1
}

// handleCreatedBlockTemplate stores a successfully created block template to
// the appropriate cache if needed, then returns the template to the miner to
// work on. The stored template is a copy of the template, to prevent races
// from occurring in case the template is mined on by the CPUminer.
// TODO, revisit the block template cache design
func handleCreatedBlockTemplate(blockTemplate *types.BlockTemplate, bm *blkmgr.BlockManager) (*types.BlockTemplate, error) {
	curTemplate := bm.GetCurrentTemplate()

	nextBlockHeight := blockTemplate.Height

	// Overwrite the old cached block if it's out of date.
	if curTemplate != nil {
		if curTemplate.Height == nextBlockHeight {
			bm.SetCurrentTemplate(blockTemplate)
		}
	}

	return blockTemplate, nil
}
