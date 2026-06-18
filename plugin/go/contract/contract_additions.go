package contract

import (
	"crypto/sha256"
	"encoding/binary"
	"math/rand"

	"google.golang.org/protobuf/types/known/anypb"
)

// ---- Status constants ----

const (
	StakeStatusActive      uint64 = 0
	StakeStatusWithdrawing uint64 = 1
	StakeStatusSlashed     uint64 = 2

	ProposalStatusVoting uint64 = 0
	ProposalStatusPassed uint64 = 1
	ProposalStatusFailed uint64 = 2
)

const (
	defaultVotingPeriodBlocks       uint64 = 10
	defaultWithdrawalCooldownBlocks uint64 = 5
	minStakeAmount                  uint64 = 1
)

// State key prefixes. Existing in this repo: 0x01 account, 0x02 pool,
// 0x07 fee params, 0x10 Plan, 0x11 PlanCounter, 0x12 Subscription,
// 0x13 SubCounter (Subex). Trust Staking uses 0x20-0x22.
var (
	trustStakePrefix    = []byte{0x20}
	slashProposalPrefix = []byte{0x21}
	stakerIndexPrefix   = []byte{0x22}
)

func KeyForTrustStake(stakerAddr, targetAddr []byte) []byte {
	return JoinLenPrefix(trustStakePrefix, stakerAddr, targetAddr)
}

func KeyForSlashProposal(proposalId []byte) []byte {
	return JoinLenPrefix(slashProposalPrefix, proposalId)
}

func KeyForStakerIndex(stakerAddr []byte) []byte {
	return JoinLenPrefix(stakerIndexPrefix, stakerAddr)
}

func computeProposalId(targetAddr, initiatorAddr []byte, height uint64) []byte {
	h := sha256.New()
	h.Write(targetAddr)
	h.Write(initiatorAddr)
	heightBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(heightBytes, height)
	h.Write(heightBytes)
	return h.Sum(nil)[:20]
}

func (c *Contract) CheckMessageStakeTrust(msg *MessageStakeTrust) *PluginCheckResponse {
	if len(msg.StakerAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if len(msg.TargetAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if msg.Amount < minStakeAmount {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	return &PluginCheckResponse{
		Recipient:         msg.TargetAddress,
		AuthorizedSigners: [][]byte{msg.StakerAddress},
	}
}

func (c *Contract) DeliverMessageStakeTrust(msg *MessageStakeTrust, fee uint64) *PluginDeliverResponse {
	var (
		stakerKey, feePoolKey, stakeKey, indexKey         []byte
		stakerBytes, feePoolBytes, stakeBytes, indexBytes []byte
		stakerQId, feeQId, stakeQId, indexQId             = rand.Uint64(), rand.Uint64(), rand.Uint64(), rand.Uint64()
		staker, feePool                                   = new(Account), new(Pool)
		stake                                             = new(TrustStake)
		index                                             = new(StakerIndex)
	)

	stakerKey = KeyForAccount(msg.StakerAddress)
	feePoolKey = KeyForFeePool(c.Config.ChainId)
	stakeKey = KeyForTrustStake(msg.StakerAddress, msg.TargetAddress)
	indexKey = KeyForStakerIndex(msg.StakerAddress)

	response, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: stakerQId, Key: stakerKey},
			{QueryId: feeQId, Key: feePoolKey},
			{QueryId: stakeQId, Key: stakeKey},
			{QueryId: indexQId, Key: indexKey},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if response.Error != nil {
		return &PluginDeliverResponse{Error: response.Error}
	}

	for _, res := range response.Results {
		if len(res.Entries) == 0 {
			continue
		}
		switch res.QueryId {
		case stakerQId:
			stakerBytes = res.Entries[0].Value
		case feeQId:
			feePoolBytes = res.Entries[0].Value
		case stakeQId:
			stakeBytes = res.Entries[0].Value
		case indexQId:
			indexBytes = res.Entries[0].Value
		}
	}

	if err = Unmarshal(stakerBytes, staker); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if err = Unmarshal(feePoolBytes, feePool); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if err = Unmarshal(stakeBytes, stake); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if err = Unmarshal(indexBytes, index); err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	amountToDeduct := msg.Amount + fee
	if staker.Amount < amountToDeduct {
		return &PluginDeliverResponse{Error: ErrInsufficientFunds()}
	}

	staker.Amount -= amountToDeduct
	feePool.Amount += fee

	isNewStake := stake.Amount == 0 && stake.Status != StakeStatusActive
	stake.StakerAddress = msg.StakerAddress
	stake.TargetAddress = msg.TargetAddress
	stake.Amount += msg.Amount
	stake.Status = StakeStatusActive
	stake.WithdrawRequestedHeight = 0
	if stake.CreatedHeight == 0 {
		stake.CreatedHeight = c.currentHeight
	}

	if isNewStake {
		index.TargetAddresses = append(index.TargetAddresses, msg.TargetAddress)
	}

	stakerBytes, err = Marshal(staker)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	feePoolBytes, err = Marshal(feePool)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	stakeBytes, err = Marshal(stake)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	indexBytes, err = Marshal(index)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	var resp *PluginStateWriteResponse
	if staker.Amount == 0 {
		resp, err = c.plugin.StateWrite(c, &PluginStateWriteRequest{
			Sets: []*PluginSetOp{
				{Key: feePoolKey, Value: feePoolBytes},
				{Key: stakeKey, Value: stakeBytes},
				{Key: indexKey, Value: indexBytes},
			},
			Deletes: []*PluginDeleteOp{{Key: stakerKey}},
		})
	} else {
		resp, err = c.plugin.StateWrite(c, &PluginStateWriteRequest{
			Sets: []*PluginSetOp{
				{Key: stakerKey, Value: stakerBytes},
				{Key: feePoolKey, Value: feePoolBytes},
				{Key: stakeKey, Value: stakeBytes},
				{Key: indexKey, Value: indexBytes},
			},
		})
	}
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if resp.Error != nil {
		return &PluginDeliverResponse{Error: resp.Error}
	}
	return &PluginDeliverResponse{}
}

func (c *Contract) CheckMessageProposeSlash(msg *MessageProposeSlash) *PluginCheckResponse {
	if len(msg.InitiatorAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if len(msg.TargetAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if len(msg.Reason) == 0 || len(msg.Reason) > 280 {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	return &PluginCheckResponse{
		Recipient:         msg.TargetAddress,
		AuthorizedSigners: [][]byte{msg.InitiatorAddress},
	}
}

func (c *Contract) DeliverMessageProposeSlash(msg *MessageProposeSlash, fee uint64) *PluginDeliverResponse {
	var (
		initiatorKey, feePoolKey, indexKey       []byte
		initiatorBytes, feePoolBytes, indexBytes []byte
		initiatorQId, feeQId, indexQId           = rand.Uint64(), rand.Uint64(), rand.Uint64()
		initiator, feePool                       = new(Account), new(Pool)
		index                                    = new(StakerIndex)
	)

	initiatorKey = KeyForAccount(msg.InitiatorAddress)
	feePoolKey = KeyForFeePool(c.Config.ChainId)
	indexKey = KeyForStakerIndex(msg.InitiatorAddress)

	response, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: initiatorQId, Key: initiatorKey},
			{QueryId: feeQId, Key: feePoolKey},
			{QueryId: indexQId, Key: indexKey},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if response.Error != nil {
		return &PluginDeliverResponse{Error: response.Error}
	}

	for _, res := range response.Results {
		if len(res.Entries) == 0 {
			continue
		}
		switch res.QueryId {
		case initiatorQId:
			initiatorBytes = res.Entries[0].Value
		case feeQId:
			feePoolBytes = res.Entries[0].Value
		case indexQId:
			indexBytes = res.Entries[0].Value
		}
	}

	if err = Unmarshal(initiatorBytes, initiator); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if err = Unmarshal(feePoolBytes, feePool); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if err = Unmarshal(indexBytes, index); err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	if len(index.TargetAddresses) == 0 {
		return &PluginDeliverResponse{Error: ErrNotEligibleToPropose()}
	}

	if initiator.Amount < fee {
		return &PluginDeliverResponse{Error: ErrInsufficientFunds()}
	}

	initiator.Amount -= fee
	feePool.Amount += fee

	proposalId := computeProposalId(msg.TargetAddress, msg.InitiatorAddress, c.currentHeight)
	proposal := &SlashProposal{
		ProposalId:           proposalId,
		TargetAddress:        msg.TargetAddress,
		InitiatorAddress:     msg.InitiatorAddress,
		Reason:               msg.Reason,
		VotesFor:             0,
		VotesAgainst:         0,
		Voters:               nil,
		Status:               ProposalStatusVoting,
		VotingDeadlineHeight: c.currentHeight + defaultVotingPeriodBlocks,
		CreatedHeight:        c.currentHeight,
	}

	initiatorBytes, err = Marshal(initiator)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	feePoolBytes, err = Marshal(feePool)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	proposalBytes, err := Marshal(proposal)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	var resp *PluginStateWriteResponse
	if initiator.Amount == 0 {
		resp, err = c.plugin.StateWrite(c, &PluginStateWriteRequest{
			Sets: []*PluginSetOp{
				{Key: feePoolKey, Value: feePoolBytes},
				{Key: KeyForSlashProposal(proposalId), Value: proposalBytes},
			},
			Deletes: []*PluginDeleteOp{{Key: initiatorKey}},
		})
	} else {
		resp, err = c.plugin.StateWrite(c, &PluginStateWriteRequest{
			Sets: []*PluginSetOp{
				{Key: initiatorKey, Value: initiatorBytes},
				{Key: feePoolKey, Value: feePoolBytes},
				{Key: KeyForSlashProposal(proposalId), Value: proposalBytes},
			},
		})
	}
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if resp.Error != nil {
		return &PluginDeliverResponse{Error: resp.Error}
	}

	eventAny, evErr := anypb.New(&SlashProposalCreatedEvent{
		ProposalId:       proposalId,
		TargetAddress:    msg.TargetAddress,
		InitiatorAddress: msg.InitiatorAddress,
	})
	if evErr != nil {
		return &PluginDeliverResponse{}
	}
	return &PluginDeliverResponse{
		Events: []*Event{
			{
				EventType: "slash_proposal_created",
				Msg:       &Event_Custom{Custom: &EventCustom{Msg: eventAny}},
				Height:    c.currentHeight,
				Address:   msg.TargetAddress,
			},
		},
	}
}

func (c *Contract) CheckMessageVoteSlash(msg *MessageVoteSlash) *PluginCheckResponse {
	if len(msg.VoterAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if len(msg.ProposalId) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidProposalId()}
	}
	return &PluginCheckResponse{
		AuthorizedSigners: [][]byte{msg.VoterAddress},
	}
}

func (c *Contract) DeliverMessageVoteSlash(msg *MessageVoteSlash, fee uint64) *PluginDeliverResponse {
	var (
		voterKey, feePoolKey, indexKey, proposalKey         []byte
		voterBytes, feePoolBytes, indexBytes, proposalBytes []byte
		voterQId, feeQId, indexQId, proposalQId             = rand.Uint64(), rand.Uint64(), rand.Uint64(), rand.Uint64()
		voter, feePool                                      = new(Account), new(Pool)
		index                                               = new(StakerIndex)
		proposal                                            = new(SlashProposal)
	)

	voterKey = KeyForAccount(msg.VoterAddress)
	feePoolKey = KeyForFeePool(c.Config.ChainId)
	indexKey = KeyForStakerIndex(msg.VoterAddress)
	proposalKey = KeyForSlashProposal(msg.ProposalId)

	response, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: voterQId, Key: voterKey},
			{QueryId: feeQId, Key: feePoolKey},
			{QueryId: indexQId, Key: indexKey},
			{QueryId: proposalQId, Key: proposalKey},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if response.Error != nil {
		return &PluginDeliverResponse{Error: response.Error}
	}

	for _, res := range response.Results {
		if len(res.Entries) == 0 {
			continue
		}
		switch res.QueryId {
		case voterQId:
			voterBytes = res.Entries[0].Value
		case feeQId:
			feePoolBytes = res.Entries[0].Value
		case indexQId:
			indexBytes = res.Entries[0].Value
		case proposalQId:
			proposalBytes = res.Entries[0].Value
		}
	}

	if len(proposalBytes) == 0 {
		return &PluginDeliverResponse{Error: ErrProposalNotFound()}
	}
	if err = Unmarshal(proposalBytes, proposal); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if proposal.Status != ProposalStatusVoting {
		return &PluginDeliverResponse{Error: ErrProposalClosed()}
	}
	if c.currentHeight > proposal.VotingDeadlineHeight {
		return &PluginDeliverResponse{Error: ErrVotingPeriodEnded()}
	}

	if err = Unmarshal(voterBytes, voter); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if err = Unmarshal(feePoolBytes, feePool); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if err = Unmarshal(indexBytes, index); err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	if len(index.TargetAddresses) == 0 {
		return &PluginDeliverResponse{Error: ErrNotEligibleToVote()}
	}

	for _, v := range proposal.Voters {
		if string(v) == string(msg.VoterAddress) {
			return &PluginDeliverResponse{Error: ErrAlreadyVoted()}
		}
	}

	if voter.Amount < fee {
		return &PluginDeliverResponse{Error: ErrInsufficientFunds()}
	}

	voter.Amount -= fee
	feePool.Amount += fee

	if msg.Approve {
		proposal.VotesFor++
	} else {
		proposal.VotesAgainst++
	}
	proposal.Voters = append(proposal.Voters, msg.VoterAddress)

	voterBytes, err = Marshal(voter)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	feePoolBytes, err = Marshal(feePool)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	proposalBytes, err = Marshal(proposal)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	var resp *PluginStateWriteResponse
	if voter.Amount == 0 {
		resp, err = c.plugin.StateWrite(c, &PluginStateWriteRequest{
			Sets: []*PluginSetOp{
				{Key: feePoolKey, Value: feePoolBytes},
				{Key: proposalKey, Value: proposalBytes},
			},
			Deletes: []*PluginDeleteOp{{Key: voterKey}},
		})
	} else {
		resp, err = c.plugin.StateWrite(c, &PluginStateWriteRequest{
			Sets: []*PluginSetOp{
				{Key: voterKey, Value: voterBytes},
				{Key: feePoolKey, Value: feePoolBytes},
				{Key: proposalKey, Value: proposalBytes},
			},
		})
	}
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if resp.Error != nil {
		return &PluginDeliverResponse{Error: resp.Error}
	}
	return &PluginDeliverResponse{}
}

func (c *Contract) CheckMessageResolveSlash(msg *MessageResolveSlash) *PluginCheckResponse {
	if len(msg.ResolverAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if len(msg.ProposalId) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidProposalId()}
	}
	return &PluginCheckResponse{
		AuthorizedSigners: [][]byte{msg.ResolverAddress},
	}
}

func (c *Contract) DeliverMessageResolveSlash(msg *MessageResolveSlash, fee uint64) *PluginDeliverResponse {
	var (
		resolverKey, feePoolKey, proposalKey       []byte
		resolverBytes, feePoolBytes, proposalBytes []byte
		resolverQId, feeQId, proposalQId           = rand.Uint64(), rand.Uint64(), rand.Uint64()
		resolver, feePool                          = new(Account), new(Pool)
		proposal                                   = new(SlashProposal)
	)

	resolverKey = KeyForAccount(msg.ResolverAddress)
	feePoolKey = KeyForFeePool(c.Config.ChainId)
	proposalKey = KeyForSlashProposal(msg.ProposalId)

	response, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: resolverQId, Key: resolverKey},
			{QueryId: feeQId, Key: feePoolKey},
			{QueryId: proposalQId, Key: proposalKey},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if response.Error != nil {
		return &PluginDeliverResponse{Error: response.Error}
	}

	for _, res := range response.Results {
		if len(res.Entries) == 0 {
			continue
		}
		switch res.QueryId {
		case resolverQId:
			resolverBytes = res.Entries[0].Value
		case feeQId:
			feePoolBytes = res.Entries[0].Value
		case proposalQId:
			proposalBytes = res.Entries[0].Value
		}
	}

	if len(proposalBytes) == 0 {
		return &PluginDeliverResponse{Error: ErrProposalNotFound()}
	}
	if err = Unmarshal(proposalBytes, proposal); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if proposal.Status != ProposalStatusVoting {
		return &PluginDeliverResponse{Error: ErrProposalClosed()}
	}
	if c.currentHeight <= proposal.VotingDeadlineHeight {
		return &PluginDeliverResponse{Error: ErrVotingPeriodNotEnded()}
	}

	if err = Unmarshal(resolverBytes, resolver); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if err = Unmarshal(feePoolBytes, feePool); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if resolver.Amount < fee {
		return &PluginDeliverResponse{Error: ErrInsufficientFunds()}
	}
	resolver.Amount -= fee
	feePool.Amount += fee

	passed := proposal.VotesFor > proposal.VotesAgainst
	if passed {
		proposal.Status = ProposalStatusPassed
	} else {
		proposal.Status = ProposalStatusFailed
	}

	resolverBytes, err = Marshal(resolver)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	proposalBytes, err = Marshal(proposal)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	sets := []*PluginSetOp{
		{Key: proposalKey, Value: proposalBytes},
	}
	var deletes []*PluginDeleteOp
	if resolver.Amount == 0 {
		deletes = append(deletes, &PluginDeleteOp{Key: resolverKey})
	} else {
		sets = append(sets, &PluginSetOp{Key: resolverKey, Value: resolverBytes})
	}

	if passed {
		slashSets, slashDeletes, slashErr := c.buildSlashWriteSet(proposal.TargetAddress, proposal.InitiatorAddress, feePool, feePoolKey)
		if slashErr != nil {
			return &PluginDeliverResponse{Error: slashErr}
		}
		sets = append(sets, slashSets...)
		deletes = append(deletes, slashDeletes...)
	} else {
		feePoolBytes, err = Marshal(feePool)
		if err != nil {
			return &PluginDeliverResponse{Error: err}
		}
		sets = append(sets, &PluginSetOp{Key: feePoolKey, Value: feePoolBytes})
	}

	resp, err := c.plugin.StateWrite(c, &PluginStateWriteRequest{Sets: sets, Deletes: deletes})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if resp.Error != nil {
		return &PluginDeliverResponse{Error: resp.Error}
	}
	return &PluginDeliverResponse{}
}

func (c *Contract) buildSlashWriteSet(targetAddr, initiatorAddr []byte, feePool *Pool, feePoolKey []byte) ([]*PluginSetOp, []*PluginDeleteOp, *PluginError) {
	rangeQId := rand.Uint64()
	rangeResp, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Ranges: []*PluginRangeRead{
			{QueryId: rangeQId, Prefix: trustStakePrefix, Limit: 0},
		},
	})
	if err != nil {
		return nil, nil, err
	}
	if rangeResp.Error != nil {
		return nil, nil, rangeResp.Error
	}

	var sets []*PluginSetOp
	var totalSlashed uint64

	var matchingEntries []*PluginStateEntry
	for _, res := range rangeResp.Results {
		if res.QueryId == rangeQId {
			matchingEntries = res.Entries
		}
	}

	for _, entry := range matchingEntries {
		stake := new(TrustStake)
		if uErr := Unmarshal(entry.Value, stake); uErr != nil {
			continue
		}
		if string(stake.TargetAddress) != string(targetAddr) {
			continue
		}
		if stake.Status != StakeStatusActive && stake.Status != StakeStatusWithdrawing {
			continue
		}
		totalSlashed += stake.Amount
		stake.Amount = 0
		stake.Status = StakeStatusSlashed
		stakeBytes, mErr := Marshal(stake)
		if mErr != nil {
			return nil, nil, mErr
		}
		sets = append(sets, &PluginSetOp{Key: entry.Key, Value: stakeBytes})
	}

	toInitiator := totalSlashed / 2
	toFeePool := totalSlashed - toInitiator
	feePool.Amount += toFeePool

	feePoolBytes, mErr := Marshal(feePool)
	if mErr != nil {
		return nil, nil, mErr
	}
	sets = append(sets, &PluginSetOp{Key: feePoolKey, Value: feePoolBytes})

	if totalSlashed == 0 {
		return sets, nil, nil
	}

	initQId := rand.Uint64()
	initResp, rErr := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{{QueryId: initQId, Key: KeyForAccount(initiatorAddr)}},
	})
	if rErr != nil {
		return nil, nil, rErr
	}
	if initResp.Error != nil {
		return nil, nil, initResp.Error
	}
	initiatorAccount := new(Account)
	for _, res := range initResp.Results {
		if res.QueryId == initQId && len(res.Entries) > 0 {
			if uErr := Unmarshal(res.Entries[0].Value, initiatorAccount); uErr != nil {
				return nil, nil, uErr
			}
		}
	}
	initiatorAccount.Amount += toInitiator
	initiatorBytes, mErr := Marshal(initiatorAccount)
	if mErr != nil {
		return nil, nil, mErr
	}
	sets = append(sets, &PluginSetOp{Key: KeyForAccount(initiatorAddr), Value: initiatorBytes})

	return sets, nil, nil
}

func (c *Contract) CheckMessageWithdrawStake(msg *MessageWithdrawStake) *PluginCheckResponse {
	if len(msg.StakerAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if len(msg.TargetAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	return &PluginCheckResponse{
		AuthorizedSigners: [][]byte{msg.StakerAddress},
	}
}

func (c *Contract) DeliverMessageWithdrawStake(msg *MessageWithdrawStake, fee uint64) *PluginDeliverResponse {
	var (
		stakerKey, feePoolKey, stakeKey, indexKey         []byte
		stakerBytes, feePoolBytes, stakeBytes, indexBytes []byte
		stakerQId, feeQId, stakeQId, indexQId             = rand.Uint64(), rand.Uint64(), rand.Uint64(), rand.Uint64()
		staker, feePool                                   = new(Account), new(Pool)
		stake                                             = new(TrustStake)
		index                                             = new(StakerIndex)
	)

	stakerKey = KeyForAccount(msg.StakerAddress)
	feePoolKey = KeyForFeePool(c.Config.ChainId)
	stakeKey = KeyForTrustStake(msg.StakerAddress, msg.TargetAddress)
	indexKey = KeyForStakerIndex(msg.StakerAddress)

	response, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: stakerQId, Key: stakerKey},
			{QueryId: feeQId, Key: feePoolKey},
			{QueryId: stakeQId, Key: stakeKey},
			{QueryId: indexQId, Key: indexKey},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if response.Error != nil {
		return &PluginDeliverResponse{Error: response.Error}
	}

	for _, res := range response.Results {
		if len(res.Entries) == 0 {
			continue
		}
		switch res.QueryId {
		case stakerQId:
			stakerBytes = res.Entries[0].Value
		case feeQId:
			feePoolBytes = res.Entries[0].Value
		case stakeQId:
			stakeBytes = res.Entries[0].Value
		case indexQId:
			indexBytes = res.Entries[0].Value
		}
	}

	if len(stakeBytes) == 0 {
		return &PluginDeliverResponse{Error: ErrStakeNotFound()}
	}
	if err = Unmarshal(stakeBytes, stake); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if stake.Status == StakeStatusSlashed {
		return &PluginDeliverResponse{Error: ErrStakeAlreadySlashed()}
	}

	if err = Unmarshal(stakerBytes, staker); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if err = Unmarshal(feePoolBytes, feePool); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if err = Unmarshal(indexBytes, index); err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	if staker.Amount < fee {
		return &PluginDeliverResponse{Error: ErrInsufficientFunds()}
	}
	staker.Amount -= fee
	feePool.Amount += fee

	sets := []*PluginSetOp{}
	var deletes []*PluginDeleteOp

	switch stake.Status {
	case StakeStatusActive:
		proposalRangeQId := rand.Uint64()
		rangeResp, rErr := c.plugin.StateRead(c, &PluginStateReadRequest{
			Ranges: []*PluginRangeRead{{QueryId: proposalRangeQId, Prefix: slashProposalPrefix, Limit: 0}},
		})
		if rErr != nil {
			return &PluginDeliverResponse{Error: rErr}
		}
		if rangeResp.Error != nil {
			return &PluginDeliverResponse{Error: rangeResp.Error}
		}
		for _, res := range rangeResp.Results {
			if res.QueryId != proposalRangeQId {
				continue
			}
			for _, entry := range res.Entries {
				p := new(SlashProposal)
				if uErr := Unmarshal(entry.Value, p); uErr != nil {
					continue
				}
				if string(p.TargetAddress) == string(msg.TargetAddress) && p.Status == ProposalStatusVoting {
					return &PluginDeliverResponse{Error: ErrActiveDisputeExists()}
				}
			}
		}

		stake.Status = StakeStatusWithdrawing
		stake.WithdrawRequestedHeight = c.currentHeight

	case StakeStatusWithdrawing:
		if c.currentHeight < stake.WithdrawRequestedHeight+defaultWithdrawalCooldownBlocks {
			return &PluginDeliverResponse{Error: ErrCooldownNotElapsed()}
		}
		staker.Amount += stake.Amount
		stake.Amount = 0

		newTargets := make([][]byte, 0, len(index.TargetAddresses))
		for _, t := range index.TargetAddresses {
			if string(t) != string(msg.TargetAddress) {
				newTargets = append(newTargets, t)
			}
		}
		index.TargetAddresses = newTargets

		indexBytes, err = Marshal(index)
		if err != nil {
			return &PluginDeliverResponse{Error: err}
		}
		sets = append(sets, &PluginSetOp{Key: indexKey, Value: indexBytes})
		deletes = append(deletes, &PluginDeleteOp{Key: stakeKey})
	}

	stakerBytes, err = Marshal(staker)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	feePoolBytes, err = Marshal(feePool)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	sets = append(sets, &PluginSetOp{Key: feePoolKey, Value: feePoolBytes})

	stakeAlreadyDeleted := false
	for _, d := range deletes {
		if string(d.Key) == string(stakeKey) {
			stakeAlreadyDeleted = true
			break
		}
	}
	if !stakeAlreadyDeleted {
		stakeBytes, err = Marshal(stake)
		if err != nil {
			return &PluginDeliverResponse{Error: err}
		}
		sets = append(sets, &PluginSetOp{Key: stakeKey, Value: stakeBytes})
	}

	if staker.Amount == 0 {
		deletes = append(deletes, &PluginDeleteOp{Key: stakerKey})
	} else {
		sets = append(sets, &PluginSetOp{Key: stakerKey, Value: stakerBytes})
	}

	resp, err := c.plugin.StateWrite(c, &PluginStateWriteRequest{Sets: sets, Deletes: deletes})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if resp.Error != nil {
		return &PluginDeliverResponse{Error: resp.Error}
	}
	return &PluginDeliverResponse{}
}
