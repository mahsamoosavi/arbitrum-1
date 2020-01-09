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

	"github.com/offchainlabs/arbitrum/packages/arb-validator/arb"
	"github.com/offchainlabs/arbitrum/packages/arb-validator/arbbridge"

	"github.com/offchainlabs/arbitrum/packages/arb-util/protocol"

	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"

	"github.com/ethereum/go-ethereum/common"
	"github.com/offchainlabs/arbitrum/packages/arb-validator/structures"
)

type ChainListener interface {
	StakeCreated(arbbridge.StakeCreatedEvent)
	StakeRemoved(arbbridge.StakeRefundedEvent)
	StakeMoved(arbbridge.StakeMovedEvent)
	StartedChallenge(arbbridge.ChallengeStartedEvent, *Node, *Node)
	CompletedChallenge(event arbbridge.ChallengeCompletedEvent)
	SawAssertion(arbbridge.AssertedEvent, *protocol.TimeBlocks, [32]byte)
	ConfirmedNode(arbbridge.ConfirmedEvent)
	PrunedLeaf(arbbridge.PrunedEvent)

	AssertionPrepared(*preparedAssertion)
	ValidNodeConfirmable(*confirmValidOpportunity)
	InvalidNodeConfirmable(*confirmInvalidOpportunity)
	PrunableLeafs([]pruneParams)
	MootableStakes([]recoverStakeMootedParams)
	OldStakes([]recoverStakeOldParams)

	AdvancedKnownValidNode([32]byte)
	AdvancedKnownAssertion(*protocol.ExecutionAssertion, [32]byte)
}

type ValidatorChainListener struct {
	chain                  *ChainObserver
	stakers                map[common.Address]*StakerListener
	broadcastAssertions    map[[32]byte]bool
	broadcastConfirmations map[[32]byte]bool
	broadcastLeafPrunes    map[[32]byte]bool
}

func NewValidatorChainListener(
	chain *ChainObserver,
) *ValidatorChainListener {
	return &ValidatorChainListener{
		chain:                  chain,
		stakers:                make(map[common.Address]*StakerListener),
		broadcastAssertions:    make(map[[32]byte]bool),
		broadcastConfirmations: make(map[[32]byte]bool),
		broadcastLeafPrunes:    make(map[[32]byte]bool),
	}
}

func (lis *ValidatorChainListener) AddStaker(client *ethclient.Client, auth *bind.TransactOpts) error {
	contract, err := arb.NewRollup(lis.chain.rollupAddr, client, auth)
	if err != nil {
		return err
	}
	location := lis.chain.knownValidNode
	proof1 := GeneratePathProof(lis.chain.nodeGraph.latestConfirmed, location)
	proof2 := GeneratePathProof(location, lis.chain.nodeGraph.getLeaf(location))
	go contract.PlaceStake(context.TODO(), lis.chain.nodeGraph.params.StakeRequirement, proof1, proof2)
	address := auth.From
	staker := &StakerListener{
		myAddr:   address,
		client:   client,
		contract: contract,
	}
	lis.stakers[address] = staker
	return nil
}

func (lis *ValidatorChainListener) StakeCreated(ev arbbridge.StakeCreatedEvent) {
	staker, ok := lis.stakers[ev.Staker]
	if ok {
		opps := lis.chain.nodeGraph.checkChallengeOpportunityAllPairs()
		for _, opp := range opps {
			go staker.initiateChallenge(context.TODO(), opp)
		}
	} else {
		lis.challengeStakerIfPossible(context.TODO(), ev.Staker)
	}
}

func (lis *ValidatorChainListener) StakeRemoved(arbbridge.StakeRefundedEvent) {

}

func (lis *ValidatorChainListener) StakeMoved(ev arbbridge.StakeMovedEvent) {
	lis.challengeStakerIfPossible(context.TODO(), ev.Staker)
}

func (lis *ValidatorChainListener) challengeStakerIfPossible(ctx context.Context, stakerAddr common.Address) {
	_, ok := lis.stakers[stakerAddr]
	if !ok {
		newStaker := lis.chain.nodeGraph.stakers.Get(stakerAddr)
		for myAddr, staker := range lis.stakers {
			meAsStaker := lis.chain.nodeGraph.stakers.Get(myAddr)
			if meAsStaker != nil {
				opp := lis.chain.nodeGraph.checkChallengeOpportunityPair(newStaker, meAsStaker)
				if opp != nil {
					staker.initiateChallenge(ctx, opp)
					return
				}
			}
			opp := lis.chain.nodeGraph.checkChallengeOpportunityAny(newStaker)
			if opp != nil {
				go staker.initiateChallenge(ctx, opp)
				return
			}
		}
	}
}

func (lis *ValidatorChainListener) StartedChallenge(ev arbbridge.ChallengeStartedEvent, conflictNode *Node, challengerAncestor *Node) {
	asserter, ok := lis.stakers[ev.Asserter]
	if ok {
		switch conflictNode.linkType {
		case structures.InvalidPendingChildType:
			go asserter.defendPendingTop(ev.ChallengeContract, lis.chain.pendingInbox, conflictNode)
		case structures.InvalidMessagesChildType:
			go asserter.defendMessages(ev.ChallengeContract, lis.chain.pendingInbox, conflictNode)
		case structures.InvalidExecutionChildType:
			go asserter.defendExecution(
				ev.ChallengeContract,
				conflictNode.machine,
				lis.chain.ExecutionPrecondition(conflictNode),
				conflictNode.disputable.AssertionParams.NumSteps,
			)
		}
	}

	challenger, ok := lis.stakers[ev.Challenger]
	if ok {
		switch conflictNode.linkType {
		case structures.InvalidPendingChildType:
			go challenger.challengePendingTop(ev.ChallengeContract, lis.chain.pendingInbox)
		case structures.InvalidMessagesChildType:
			go challenger.challengeMessages(ev.ChallengeContract, lis.chain.pendingInbox, conflictNode)
		case structures.InvalidExecutionChildType:
			go challenger.challengeExecution(
				ev.ChallengeContract,
				conflictNode.machine,
				lis.chain.ExecutionPrecondition(conflictNode),
			)
		}
	}
}

func (lis *ValidatorChainListener) CompletedChallenge(ev arbbridge.ChallengeCompletedEvent) {
	_, ok := lis.stakers[ev.Winner]
	if ok {
		lis.wonChallenge(ev)
	}

	_, ok = lis.stakers[ev.Loser]
	if ok {
		lis.lostChallenge(ev)
	}
	lis.challengeStakerIfPossible(context.TODO(), ev.Winner)
}

func (lis *ValidatorChainListener) lostChallenge(arbbridge.ChallengeCompletedEvent) {

}

func (lis *ValidatorChainListener) wonChallenge(arbbridge.ChallengeCompletedEvent) {

}

func (lis *ValidatorChainListener) SawAssertion(arbbridge.AssertedEvent, *protocol.TimeBlocks, [32]byte) {

}

func (lis *ValidatorChainListener) ConfirmedNode(arbbridge.ConfirmedEvent) {

}

func (lis *ValidatorChainListener) PrunedLeaf(arbbridge.PrunedEvent) {

}

func (lis *ValidatorChainListener) AssertionPrepared(prepared *preparedAssertion) {
	_, alreadySent := lis.broadcastAssertions[prepared.leafHash]
	if alreadySent {
		return
	}
	leaf, ok := lis.chain.nodeGraph.nodeFromHash[prepared.leafHash]
	if ok {
		for _, staker := range lis.stakers {
			stakerPos := lis.chain.nodeGraph.stakers.Get(staker.myAddr)
			if stakerPos != nil {
				proof := GeneratePathProof(stakerPos.location, leaf)
				if proof != nil {
					lis.broadcastAssertions[prepared.leafHash] = true
					go func() {
						err := staker.makeAssertion(context.TODO(), prepared, proof)
						if err != nil {
							log.Println("Error making assertion", err)
						} else {
							log.Println("Successfully made assertion")
						}
					}()

					break
				}
			}
		}
	}
}

func (lis *ValidatorChainListener) ValidNodeConfirmable(conf *confirmValidOpportunity) {
	_, alreadySent := lis.broadcastConfirmations[conf.nodeHash]
	if alreadySent {
		return
	}
	for _, staker := range lis.stakers {
		lis.broadcastConfirmations[conf.nodeHash] = true
		go func() {
			staker.Lock()
			staker.contract.ConfirmValid(
				context.TODO(),
				conf.deadlineTicks,
				conf.messages,
				conf.logsAcc,
				conf.vmProtoStateHash,
				conf.stakerAddresses,
				conf.stakerProofs,
				conf.stakerProofOffsets,
			)
			staker.Unlock()
		}()
		break
	}
}

func (lis *ValidatorChainListener) InvalidNodeConfirmable(conf *confirmInvalidOpportunity) {
	_, alreadySent := lis.broadcastConfirmations[conf.nodeHash]
	if alreadySent {
		return
	}
	for _, staker := range lis.stakers {
		lis.broadcastConfirmations[conf.nodeHash] = true
		go func() {
			staker.Lock()
			staker.contract.ConfirmInvalid(
				context.TODO(),
				conf.deadlineTicks,
				conf.challengeNodeData,
				conf.branch,
				conf.vmProtoStateHash,
				conf.stakerAddresses,
				conf.stakerProofs,
				conf.stakerProofOffsets,
			)
			staker.Unlock()
		}()
		break
	}
}

func (lis *ValidatorChainListener) PrunableLeafs(params []pruneParams) {
	for _, staker := range lis.stakers {
		for _, prune := range params {
			_, alreadySent := lis.broadcastLeafPrunes[prune.leafHash]
			if alreadySent {
				continue
			}
			lis.broadcastLeafPrunes[prune.leafHash] = true
			pruneCopy := prune.Clone()
			go func() {
				staker.Lock()
				staker.contract.PruneLeaf(
					context.TODO(),
					pruneCopy.ancestorHash,
					pruneCopy.leafProof,
					pruneCopy.ancProof,
				)
				staker.Unlock()
			}()
		}
		break
	}
}

func (lis *ValidatorChainListener) MootableStakes(params []recoverStakeMootedParams) {
	for _, staker := range lis.stakers {
		for _, moot := range params {
			go func() {
				staker.Lock()
				staker.contract.RecoverStakeMooted(
					context.TODO(),
					moot.ancestorHash,
					moot.addr,
					moot.lcProof,
					moot.stProof,
				)
				staker.Unlock()
			}()
		}
		break
	}
}

func (lis *ValidatorChainListener) OldStakes(params []recoverStakeOldParams) {
	for _, staker := range lis.stakers {
		for _, old := range params {
			go func() {
				staker.Lock()
				staker.contract.RecoverStakeOld(
					context.TODO(),
					old.addr,
					old.proof,
				)
				staker.Unlock()
			}()
		}
		break
	}
}

func (lis *ValidatorChainListener) AdvancedKnownValidNode([32]byte)                               {}
func (lis *ValidatorChainListener) AdvancedKnownAssertion(*protocol.ExecutionAssertion, [32]byte) {}
