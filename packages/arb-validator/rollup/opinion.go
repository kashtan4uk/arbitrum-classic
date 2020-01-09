/*
* Copyright 2020, Offchain Labs, Inc.
*
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You may obtain a copy of the License at
*
*    http://www.apache.org/licenses/LICENSE-2.0
*
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
 */

package rollup

import (
	"context"
	"log"
	"math/big"
	"time"

	"github.com/offchainlabs/arbitrum/packages/arb-util/protocol"

	"github.com/offchainlabs/arbitrum/packages/arb-util/machine"
	"github.com/offchainlabs/arbitrum/packages/arb-util/value"
	"github.com/offchainlabs/arbitrum/packages/arb-validator/structures"
)

type preparedAssertion struct {
	leafHash         [32]byte
	prevPrevLeafHash [32]byte
	prevDataHash     [32]byte
	prevDeadline     structures.TimeTicks
	prevChildType    structures.ChildType

	beforeState *structures.VMProtoData
	params      *structures.AssertionParams
	claim       *structures.AssertionClaim
	assertion   *protocol.ExecutionAssertion
	machine     machine.Machine
}

func (pa *preparedAssertion) Clone() *preparedAssertion {
	return &preparedAssertion{
		leafHash:         pa.leafHash,
		prevPrevLeafHash: pa.prevPrevLeafHash,
		prevDataHash:     pa.prevDataHash,
		prevDeadline:     structures.TimeTicks{new(big.Int).Set(pa.prevDeadline.Val)},
		prevChildType:    pa.prevChildType,
		beforeState:      pa.beforeState.Clone(),
		params:           pa.params.Clone(),
		claim:            pa.claim.Clone(),
		assertion:        pa.assertion,
		machine:          pa.machine,
	}
}

func (chain *ChainObserver) startOpinionUpdateThread(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Second)
		assertionPreparedChan := make(chan *preparedAssertion, 20)
		preparingAssertions := make(map[[32]byte]bool)
		preparedAssertions := make(map[[32]byte]*preparedAssertion)

		updateCurrent := func() {
			currentOpinion := chain.knownValidNode
			successorHashes := [4][32]byte{}
			copy(successorHashes[:], currentOpinion.successorHashes[:])
			successor := func() *Node {
				for _, successor := range currentOpinion.successorHashes {
					if successor != zeroBytes32 {
						return chain.nodeGraph.nodeFromHash[successor]
					}
				}
				return nil
			}()

			var newOpinion structures.ChildType
			var nextMachine machine.Machine
			var validExecution *protocol.ExecutionAssertion
			prepped, found := preparedAssertions[currentOpinion.hash]

			if found &&
				prepped.params.Equals(successor.disputable.AssertionParams) &&
				prepped.claim.Equals(successor.disputable.AssertionClaim) {
				newOpinion = structures.ValidChildType
				nextMachine = prepped.machine
				validExecution = prepped.assertion
				chain.RUnlock()
			} else {
				params := successor.disputable.AssertionParams.Clone()
				claim := successor.disputable.AssertionClaim.Clone()
				claimHeight, found := chain.pendingInbox.GetHeight(claim.AfterPendingTop)
				var claimHeightCopy *big.Int
				if found {
					claimHeightCopy = new(big.Int).Set(claimHeight)
				}
				messageStack, _ := chain.pendingInbox.Substack(currentOpinion.vmProtoData.PendingTop, claim.AfterPendingTop)
				messagesVal := chain.pendingInbox.ValueForSubseq(currentOpinion.vmProtoData.PendingTop, claim.AfterPendingTop)
				nextMachine = currentOpinion.machine.Clone()
				prevPendingCount := new(big.Int).Set(currentOpinion.vmProtoData.PendingCount)
				chain.RUnlock()

				newOpinion, validExecution = getNodeOpinion(params, claim, prevPendingCount, claimHeightCopy, messageStack, messagesVal, nextMachine)
			}
			// Reset prepared
			preparingAssertions = make(map[[32]byte]bool)
			preparedAssertions = make(map[[32]byte]*preparedAssertion)

			chain.RLock()
			correctNode, ok := chain.nodeGraph.nodeFromHash[successorHashes[newOpinion]]
			if ok {
				if newOpinion == structures.ValidChildType {
					for _, lis := range chain.listeners {
						lis.AdvancedKnownAssertion(validExecution, correctNode.assertionTxHash)
					}
				}
				chain.RUnlock()
				chain.Lock()
				if newOpinion == structures.ValidChildType {
					correctNode.machine = nextMachine
					correctNode.assertion = validExecution
				} else {
					correctNode.machine = chain.knownValidNode.machine.Clone()
				}
				chain.knownValidNode = correctNode
				chain.Unlock()
				chain.RLock()
				for _, listener := range chain.listeners {
					listener.AdvancedKnownValidNode(chain.knownValidNode.hash)
				}

			}
			chain.RUnlock()

		}

		for {
			select {
			case <-ctx.Done():
				break
			case prepped := <-assertionPreparedChan:
				preparedAssertions[prepped.leafHash] = prepped
			case <-ticker.C:
				chain.RLock()
				// Catch up to current head
				for !chain.nodeGraph.leaves.IsLeaf(chain.knownValidNode) {
					updateCurrent()
					chain.RLock()
				}
				// Prepare next assertion
				_, isPreparing := preparingAssertions[chain.knownValidNode.hash]
				if !isPreparing {
					newMessages := chain.knownValidNode.vmProtoData.PendingTop != chain.pendingInbox.GetTopHash()
					if !machine.IsMachineBlocked(chain.knownValidNode.machine, chain.latestBlockNumber, newMessages) {
						preparingAssertions[chain.knownValidNode.hash] = true
						go func() {
							assertionPreparedChan <- chain.prepareAssertion()
						}()
					}
				} else {
					prepared, isPrepared := preparedAssertions[chain.knownValidNode.hash]
					if isPrepared && chain.nodeGraph.leaves.IsLeaf(chain.knownValidNode) {
						for _, lis := range chain.listeners {
							lis.AssertionPrepared(prepared.Clone())
						}
					}
				}
				chain.RUnlock()

			}
		}
	}()
}

func (chain *ChainObserver) prepareAssertion() *preparedAssertion {
	chain.RLock()
	currentOpinion := chain.knownValidNode
	currentOpinionHash := currentOpinion.hash
	prevPrevLeafHash := currentOpinion.PrevHash()
	prevDataHash := currentOpinion.nodeDataHash
	prevDeadline := structures.TimeTicks{new(big.Int).Set(currentOpinion.deadline.Val)}
	prevChildType := currentOpinion.linkType
	beforeState := currentOpinion.vmProtoData.Clone()
	if !chain.nodeGraph.leaves.IsLeaf(currentOpinion) {
		return nil
	}
	afterPendingTop := chain.pendingInbox.GetTopHash()
	beforePendingTop := beforeState.PendingTop
	messageStack, _ := chain.pendingInbox.Substack(beforePendingTop, afterPendingTop)
	messagesVal := chain.pendingInbox.ValueForSubseq(beforePendingTop, afterPendingTop)
	mach := currentOpinion.machine.Clone()
	timeBounds := chain.currentTimeBounds()
	chain.RUnlock()

	assertion, stepsRun := mach.ExecuteAssertion(chain.nodeGraph.params.MaxExecutionSteps, timeBounds, messagesVal)

	log.Println("Prepared assertion of", stepsRun, "steps, ending with", mach.LastBlockReason())
	var params *structures.AssertionParams
	var claim *structures.AssertionClaim
	if assertion.DidInboxInsn {
		params = &structures.AssertionParams{
			NumSteps:             stepsRun,
			TimeBounds:           timeBounds,
			ImportedMessageCount: messageStack.TopCount(),
		}
		claim = &structures.AssertionClaim{
			AfterPendingTop:       afterPendingTop,
			ImportedMessagesSlice: messageStack.GetTopHash(),
			AssertionStub:         assertion.Stub(),
		}
	} else {
		params = &structures.AssertionParams{
			NumSteps:             stepsRun,
			TimeBounds:           timeBounds,
			ImportedMessageCount: big.NewInt(0),
		}
		claim = &structures.AssertionClaim{
			AfterPendingTop:       beforePendingTop,
			ImportedMessagesSlice: value.NewEmptyTuple().Hash(),
			AssertionStub:         assertion.Stub(),
		}
	}
	return &preparedAssertion{
		leafHash:         currentOpinionHash,
		prevPrevLeafHash: prevPrevLeafHash,
		prevDataHash:     prevDataHash,
		prevDeadline:     prevDeadline,
		prevChildType:    prevChildType,
		beforeState:      beforeState,
		params:           params,
		claim:            claim,
		assertion:        assertion,
		machine:          mach,
	}
}

func getNodeOpinion(
	params *structures.AssertionParams,
	claim *structures.AssertionClaim,
	prevPendingCount *big.Int,
	claimHeight *big.Int,
	messageStack *structures.MessageStack,
	messagesVal value.TupleValue,
	prevMach machine.Machine,
) (structures.ChildType, *protocol.ExecutionAssertion) {
	correctAfterPendingTopHeight := new(big.Int).Add(prevPendingCount, params.ImportedMessageCount)
	if claimHeight == nil || correctAfterPendingTopHeight.Cmp(claimHeight) != 0 {
		return structures.InvalidPendingChildType, nil
	}
	if messageStack.GetTopHash() != claim.ImportedMessagesSlice {
		return structures.InvalidMessagesChildType, nil
	}

	mach := prevMach
	assertion, stepsRun := mach.ExecuteAssertion(params.NumSteps, params.TimeBounds, messagesVal)
	if params.NumSteps != stepsRun || !claim.AssertionStub.Equals(assertion.Stub()) {
		return structures.InvalidExecutionChildType, nil
	}

	return structures.ValidChildType, assertion
}
