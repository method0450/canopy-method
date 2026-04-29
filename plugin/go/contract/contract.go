package contract

import (
	"bytes"
	"encoding/binary"
	"log"
	"math/rand"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"
)

// ─── Plugin Configuration ─────────────────────────────────────────────────────
// SupportedTransactions[i] must exactly correspond to TransactionTypeUrls[i].

var ContractConfig = &PluginConfig{
	Name:    "subex_plugin",
	Id:      1,
	Version: 1,
	SupportedTransactions: []string{
		"send",
		"create_plan",
		"subscribe",
		"process_billing",
		"cancel_subscription",
		"pause_subscription",
		"resume_subscription",
		"update_plan",
	},
	TransactionTypeUrls: []string{
		"type.googleapis.com/types.MessageSend",
		"type.googleapis.com/types.MessageCreatePlan",
		"type.googleapis.com/types.MessageSubscribe",
		"type.googleapis.com/types.MessageProcessBilling",
		"type.googleapis.com/types.MessageCancelSubscription",
		"type.googleapis.com/types.MessagePauseSubscription",
		"type.googleapis.com/types.MessageResumeSubscription",
		"type.googleapis.com/types.MessageUpdatePlan",
	},
	EventTypeUrls: nil,
}

// init registers all protobuf file descriptors with the FSM config.
// This is REQUIRED — without it the FSM handshake fails because the FSM
// cannot deserialize your custom message types.
func init() {
	// Explicitly initialize proto files first to ensure File_*_proto vars are set
	file_account_proto_init()
	file_event_proto_init()
	file_plugin_proto_init()
	file_tx_proto_init()

	var fds [][]byte
	// Include google/protobuf/any.proto first — it is a dependency of event.proto and tx.proto
	for _, file := range []protoreflect.FileDescriptor{
		anypb.File_google_protobuf_any_proto,
		File_account_proto, File_event_proto, File_plugin_proto, File_tx_proto,
	} {
		fd, _ := proto.Marshal(protodesc.ToFileDescriptorProto(file))
		fds = append(fds, fd)
	}
	ContractConfig.FileDescriptorProtos = fds
}

// ─── Contract Struct ──────────────────────────────────────────────────────────

type Contract struct {
	Config        Config
	FSMConfig     *PluginFSMConfig
	plugin        *Plugin
	fsmId         uint64
	currentHeight uint64 // captured in BeginBlock — PluginDeliverRequest carries no height field
}

// ─── State Key Prefixes ───────────────────────────────────────────────────────
// Built-in prefixes (must not reuse):
//   []byte{1}  Account
//   []byte{2}  Pool / FeePool
//   []byte{7}  FeeParams (governance params)
// Subex-specific prefixes:
//   []byte{0x10}  Plan
//   []byte{0x11}  PlanCounter
//   []byte{0x12}  Subscription
//   []byte{0x13}  SubCounter

var (
	accountPrefix     = []byte{1}    // matches template
	poolPrefix        = []byte{2}    // matches template
	paramsPrefix      = []byte{7}    // matches template
	planPrefix        = []byte{0x10}
	planCounterPrefix = []byte{0x11}
	subPrefix         = []byte{0x12}
	subCounterPrefix  = []byte{0x13}
)

// Billing reward paid to processors per successful cycle (1 SBX = 1_000_000 uSBX)
const BillingReward uint64 = 1_000_000

// BlocksPerDay at ~24s block time
const BlocksPerDay uint64 = 3600

// ─── Key Functions ────────────────────────────────────────────────────────────

// KeyForAccount matches the template exactly
func KeyForAccount(addr []byte) []byte {
	return JoinLenPrefix(accountPrefix, addr)
}

// KeyForFeePool matches the template exactly
func KeyForFeePool(chainId uint64) []byte {
	return JoinLenPrefix(poolPrefix, formatUint64(chainId))
}

// KeyForFeeParams matches the template exactly
func KeyForFeeParams() []byte {
	return JoinLenPrefix(paramsPrefix, []byte("/f/"))
}

func KeyForPlan(id uint64) []byte {
	return JoinLenPrefix(planPrefix, formatUint64(id))
}

func KeyForPlanCounter() []byte {
	return JoinLenPrefix(planCounterPrefix, []byte("/pc/"))
}

func KeyForSubscription(id uint64) []byte {
	return JoinLenPrefix(subPrefix, formatUint64(id))
}

func KeyForSubCounter() []byte {
	return JoinLenPrefix(subCounterPrefix, []byte("/sc/"))
}

func formatUint64(u uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, u)
	return b
}

// ─── Lifecycle: Genesis ───────────────────────────────────────────────────────

func (c *Contract) Genesis(_ *PluginGenesisRequest) *PluginGenesisResponse {
	// Seed FeeParams with Subex defaults (all fields including Subex-specific fees)
	fp := &FeeParams{
		SendFee:           10_000,
		CreatePlanFee:     50_000,
		SubscribeFee:      10_000,
		ProcessBillingFee: 5_000,
		CancelFee:         10_000,
		UpdatePlanFee:     20_000,
		PauseFee:          10_000,
	}
	fpBytes, err := Marshal(fp)
	if err != nil {
		return &PluginGenesisResponse{Error: err}
	}
	writeResp, pluginErr := c.plugin.StateWrite(c, &PluginStateWriteRequest{
		Sets: []*PluginSetOp{
			{Key: KeyForFeeParams(), Value: fpBytes},
		},
	})
	if pluginErr != nil {
		return &PluginGenesisResponse{Error: pluginErr}
	}
	if writeResp.Error != nil {
		return &PluginGenesisResponse{Error: writeResp.Error}
	}
	return &PluginGenesisResponse{}
}

// ─── Lifecycle: BeginBlock ────────────────────────────────────────────────────

func (c *Contract) BeginBlock(request *PluginBeginRequest) *PluginBeginResponse {
	// PluginDeliverRequest has no height field — capture it here for use in DeliverTx
	c.currentHeight = request.Height
	return &PluginBeginResponse{}
}

// ─── Lifecycle: CheckTx ───────────────────────────────────────────────────────

func (c *Contract) CheckTx(request *PluginCheckRequest) *PluginCheckResponse {
	// Read fee params — same pattern as template
	resp, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: rand.Uint64(), Key: KeyForFeeParams()},
		},
	})
	if err == nil {
		err = resp.Error
	}
	if err != nil {
		return &PluginCheckResponse{Error: err}
	}
	minFees := new(FeeParams)
	if err = Unmarshal(resp.Results[0].Entries[0].Value, minFees); err != nil {
		return &PluginCheckResponse{Error: err}
	}

	// Dispatch to per-type fee check then message check
	msg, err := FromAny(request.Tx.Msg)
	if err != nil {
		return &PluginCheckResponse{Error: err}
	}

	switch x := msg.(type) {
	case *MessageSend:
		if request.Tx.Fee < minFees.SendFee {
			return &PluginCheckResponse{Error: ErrTxFeeBelowStateLimit()}
		}
		return c.CheckMessageSend(x)
	case *MessageCreatePlan:
		if request.Tx.Fee < minFees.CreatePlanFee {
			return &PluginCheckResponse{Error: ErrTxFeeBelowStateLimit()}
		}
		return c.CheckCreatePlan(x)
	case *MessageSubscribe:
		if request.Tx.Fee < minFees.SubscribeFee {
			return &PluginCheckResponse{Error: ErrTxFeeBelowStateLimit()}
		}
		return c.CheckSubscribe(x)
	case *MessageProcessBilling:
		if request.Tx.Fee < minFees.ProcessBillingFee {
			return &PluginCheckResponse{Error: ErrTxFeeBelowStateLimit()}
		}
		return c.CheckProcessBilling(x)
	case *MessageCancelSubscription:
		if request.Tx.Fee < minFees.CancelFee {
			return &PluginCheckResponse{Error: ErrTxFeeBelowStateLimit()}
		}
		return c.CheckCancelSubscription(x)
	case *MessagePauseSubscription:
		if request.Tx.Fee < minFees.PauseFee {
			return &PluginCheckResponse{Error: ErrTxFeeBelowStateLimit()}
		}
		return c.CheckPauseSubscription(x)
	case *MessageResumeSubscription:
		if request.Tx.Fee < minFees.SubscribeFee {
			return &PluginCheckResponse{Error: ErrTxFeeBelowStateLimit()}
		}
		return c.CheckResumeSubscription(x)
	case *MessageUpdatePlan:
		if request.Tx.Fee < minFees.UpdatePlanFee {
			return &PluginCheckResponse{Error: ErrTxFeeBelowStateLimit()}
		}
		return c.CheckUpdatePlan(x)
	default:
		return &PluginCheckResponse{Error: ErrInvalidMessageCast()}
	}
}

// ─── CheckTx Handlers ─────────────────────────────────────────────────────────

func (c *Contract) CheckMessageSend(msg *MessageSend) *PluginCheckResponse {
	if len(msg.FromAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if len(msg.ToAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if msg.Amount == 0 {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	return &PluginCheckResponse{Recipient: msg.ToAddress, AuthorizedSigners: [][]byte{msg.FromAddress}}
}

func (c *Contract) CheckCreatePlan(msg *MessageCreatePlan) *PluginCheckResponse {
	if len(msg.CreatorAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if len(msg.Name) == 0 || len(msg.Name) > 64 {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	if len(msg.Description) > 280 {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	if msg.Price == 0 {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	if msg.IntervalDays == 0 {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	return &PluginCheckResponse{AuthorizedSigners: [][]byte{msg.CreatorAddress}}
}

func (c *Contract) CheckSubscribe(msg *MessageSubscribe) *PluginCheckResponse {
	if len(msg.SubscriberAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if msg.PlanId == 0 {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	return &PluginCheckResponse{AuthorizedSigners: [][]byte{msg.SubscriberAddress}}
}

func (c *Contract) CheckProcessBilling(msg *MessageProcessBilling) *PluginCheckResponse {
	if len(msg.ProcessorAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if msg.SubscriptionId == 0 {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	return &PluginCheckResponse{AuthorizedSigners: [][]byte{msg.ProcessorAddress}}
}

func (c *Contract) CheckCancelSubscription(msg *MessageCancelSubscription) *PluginCheckResponse {
	if len(msg.SubscriberAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if msg.SubscriptionId == 0 {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	return &PluginCheckResponse{AuthorizedSigners: [][]byte{msg.SubscriberAddress}}
}

func (c *Contract) CheckPauseSubscription(msg *MessagePauseSubscription) *PluginCheckResponse {
	if len(msg.SubscriberAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if msg.SubscriptionId == 0 {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	return &PluginCheckResponse{AuthorizedSigners: [][]byte{msg.SubscriberAddress}}
}

func (c *Contract) CheckResumeSubscription(msg *MessageResumeSubscription) *PluginCheckResponse {
	if len(msg.SubscriberAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if msg.SubscriptionId == 0 {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	return &PluginCheckResponse{AuthorizedSigners: [][]byte{msg.SubscriberAddress}}
}

func (c *Contract) CheckUpdatePlan(msg *MessageUpdatePlan) *PluginCheckResponse {
	if len(msg.CreatorAddress) != 20 {
		return &PluginCheckResponse{Error: ErrInvalidAddress()}
	}
	if msg.PlanId == 0 {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	if len(msg.Name) > 64 {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	if len(msg.Description) > 280 {
		return &PluginCheckResponse{Error: ErrInvalidAmount()}
	}
	return &PluginCheckResponse{AuthorizedSigners: [][]byte{msg.CreatorAddress}}
}

// ─── Lifecycle: DeliverTx ─────────────────────────────────────────────────────

func (c *Contract) DeliverTx(request *PluginDeliverRequest) *PluginDeliverResponse {
	msg, err := FromAny(request.Tx.Msg)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	switch x := msg.(type) {
	case *MessageSend:
		return c.DeliverMessageSend(x, request.Tx.Fee)
	case *MessageCreatePlan:
		return c.DeliverCreatePlan(x, request.Tx.Fee)
	case *MessageSubscribe:
		return c.DeliverSubscribe(x, request.Tx.Fee)
	case *MessageProcessBilling:
		return c.DeliverProcessBilling(x, request.Tx.Fee)
	case *MessageCancelSubscription:
		return c.DeliverCancelSubscription(x, request.Tx.Fee)
	case *MessagePauseSubscription:
		return c.DeliverPauseSubscription(x, request.Tx.Fee)
	case *MessageResumeSubscription:
		return c.DeliverResumeSubscription(x, request.Tx.Fee)
	case *MessageUpdatePlan:
		return c.DeliverUpdatePlan(x, request.Tx.Fee)
	default:
		return &PluginDeliverResponse{Error: ErrInvalidMessageCast()}
	}
}

// ─── DeliverTx: MessageSend ───────────────────────────────────────────────────
// Matches template pattern exactly, including self-transfer guard and zero-balance delete.

func (c *Contract) DeliverMessageSend(msg *MessageSend, fee uint64) *PluginDeliverResponse {
	log.Printf("DeliverMessageSend: from=%x to=%x amount=%d fee=%d", msg.FromAddress, msg.ToAddress, msg.Amount, fee)
	var (
		fromKey, toKey, feePoolKey             = KeyForAccount(msg.FromAddress), KeyForAccount(msg.ToAddress), KeyForFeePool(c.Config.ChainId)
		fromQueryId, toQueryId, feeQueryId     = rand.Uint64(), rand.Uint64(), rand.Uint64()
		from, to, feePool                      = new(Account), new(Account), new(Pool)
		fromBytes, toBytes, feePoolBytes       []byte
	)

	response, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: feeQueryId, Key: feePoolKey},
			{QueryId: fromQueryId, Key: fromKey},
			{QueryId: toQueryId, Key: toKey},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if response.Error != nil {
		return &PluginDeliverResponse{Error: response.Error}
	}

	for _, resp := range response.Results {
		if len(resp.Entries) == 0 {
			continue
		}
		switch resp.QueryId {
		case fromQueryId:
			fromBytes = resp.Entries[0].Value
		case toQueryId:
			toBytes = resp.Entries[0].Value
		case feeQueryId:
			feePoolBytes = resp.Entries[0].Value
		}
	}

	if err = Unmarshal(fromBytes, from); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if err = Unmarshal(toBytes, to); err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if err = Unmarshal(feePoolBytes, feePool); err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	amountToDeduct := msg.Amount + fee
	if from.Amount < amountToDeduct {
		return &PluginDeliverResponse{Error: ErrInsufficientFunds()}
	}
	// Handle self-transfer
	if bytes.Equal(fromKey, toKey) {
		to = from
	}
	from.Amount -= amountToDeduct
	feePool.Amount += fee
	to.Amount += msg.Amount

	fromBytes, err = Marshal(from)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	toBytes, err = Marshal(to)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	feePoolBytes, err = Marshal(feePool)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	var writeResp *PluginStateWriteResponse
	if from.Amount == 0 {
		writeResp, err = c.plugin.StateWrite(c, &PluginStateWriteRequest{
			Sets:    []*PluginSetOp{{Key: feePoolKey, Value: feePoolBytes}, {Key: toKey, Value: toBytes}},
			Deletes: []*PluginDeleteOp{{Key: fromKey}},
		})
	} else {
		writeResp, err = c.plugin.StateWrite(c, &PluginStateWriteRequest{
			Sets: []*PluginSetOp{
				{Key: feePoolKey, Value: feePoolBytes},
				{Key: toKey, Value: toBytes},
				{Key: fromKey, Value: fromBytes},
			},
		})
	}
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if writeResp.Error != nil {
		return &PluginDeliverResponse{Error: writeResp.Error}
	}
	return &PluginDeliverResponse{}
}

// ─── DeliverTx: CreatePlan ────────────────────────────────────────────────────

func (c *Contract) DeliverCreatePlan(msg *MessageCreatePlan, fee uint64) *PluginDeliverResponse {
	log.Printf("DeliverCreatePlan: creator=%x name=%s fee=%d", msg.CreatorAddress, msg.Name, fee)

	var (
		counterQId  = rand.Uint64()
		creatorQId  = rand.Uint64()
		feeQId      = rand.Uint64()
		counter     = new(PlanCounter)
		creatorAcc  = new(Account)
		feePool     = new(Pool)
	)

	resp, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: counterQId, Key: KeyForPlanCounter()},
			{QueryId: creatorQId, Key: KeyForAccount(msg.CreatorAddress)},
			{QueryId: feeQId, Key: KeyForFeePool(c.Config.ChainId)},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if resp.Error != nil {
		return &PluginDeliverResponse{Error: resp.Error}
	}

	for _, r := range resp.Results {
		if len(r.Entries) == 0 {
			continue
		}
		switch r.QueryId {
		case counterQId:
			if err = Unmarshal(r.Entries[0].Value, counter); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		case creatorQId:
			if err = Unmarshal(r.Entries[0].Value, creatorAcc); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		case feeQId:
			if err = Unmarshal(r.Entries[0].Value, feePool); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		}
	}

	if creatorAcc.Amount < fee {
		return &PluginDeliverResponse{Error: ErrInsufficientFunds()}
	}

	counter.Count++
	plan := &Plan{
		Id:             counter.Count,
		CreatorAddress: msg.CreatorAddress,
		Name:           msg.Name,
		Description:    msg.Description,
		Price:          msg.Price,
		IntervalDays:   msg.IntervalDays,
		TrialDays:      msg.TrialDays,
		MaxSubscribers: msg.MaxSubscribers,
		ActiveSubs:     0,
		CreatedHeight:  c.currentHeight,
		Active:         true,
	}

	creatorAcc.Amount -= fee
	feePool.Amount += fee

	planBytes, err := Marshal(plan)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	counterBytes, err := Marshal(counter)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	creatorBytes, err := Marshal(creatorAcc)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	feeBytes, err := Marshal(feePool)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	writeResp, err := c.plugin.StateWrite(c, &PluginStateWriteRequest{
		Sets: []*PluginSetOp{
			{Key: KeyForPlan(plan.Id), Value: planBytes},
			{Key: KeyForPlanCounter(), Value: counterBytes},
			{Key: KeyForAccount(msg.CreatorAddress), Value: creatorBytes},
			{Key: KeyForFeePool(c.Config.ChainId), Value: feeBytes},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if writeResp.Error != nil {
		return &PluginDeliverResponse{Error: writeResp.Error}
	}
	log.Printf("CreatePlan success: planId=%d", plan.Id)
	return &PluginDeliverResponse{}
}

// ─── DeliverTx: Subscribe ─────────────────────────────────────────────────────

func (c *Contract) DeliverSubscribe(msg *MessageSubscribe, fee uint64) *PluginDeliverResponse {
	log.Printf("DeliverSubscribe: subscriber=%x planId=%d fee=%d", msg.SubscriberAddress, msg.PlanId, fee)

	var (
		subCounterQId = rand.Uint64()
		planQId       = rand.Uint64()
		subAccQId     = rand.Uint64()
		feeQId        = rand.Uint64()
		subCounter    = new(SubCounter)
		plan          = new(Plan)
		subAcc        = new(Account)
		feePool       = new(Pool)
	)

	resp, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: subCounterQId, Key: KeyForSubCounter()},
			{QueryId: planQId, Key: KeyForPlan(msg.PlanId)},
			{QueryId: subAccQId, Key: KeyForAccount(msg.SubscriberAddress)},
			{QueryId: feeQId, Key: KeyForFeePool(c.Config.ChainId)},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if resp.Error != nil {
		return &PluginDeliverResponse{Error: resp.Error}
	}

	for _, r := range resp.Results {
		if len(r.Entries) == 0 {
			continue
		}
		switch r.QueryId {
		case subCounterQId:
			if err = Unmarshal(r.Entries[0].Value, subCounter); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		case planQId:
			if err = Unmarshal(r.Entries[0].Value, plan); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		case subAccQId:
			if err = Unmarshal(r.Entries[0].Value, subAcc); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		case feeQId:
			if err = Unmarshal(r.Entries[0].Value, feePool); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		}
	}

	// Plan must exist and be active
	if plan.Id == 0 || !plan.Active {
		return &PluginDeliverResponse{Error: ErrInvalidAmount()}
	}
	// Check cap
	if plan.MaxSubscribers > 0 && plan.ActiveSubs >= plan.MaxSubscribers {
		return &PluginDeliverResponse{Error: ErrInvalidAmount()}
	}

	// Determine first payment requirement
	firstPayment := uint64(0)
	if plan.TrialDays == 0 {
		firstPayment = plan.Price
	}
	if subAcc.Amount < fee+firstPayment {
		return &PluginDeliverResponse{Error: ErrInsufficientFunds()}
	}

	// Build subscription
	status := "active"
	trialEndsHeight := uint64(0)
	nextBillingHeight := c.currentHeight + (plan.IntervalDays * BlocksPerDay)
	billingCycle := uint64(0)

	if plan.TrialDays > 0 {
		status = "trial"
		trialEndsHeight = c.currentHeight + (plan.TrialDays * BlocksPerDay)
		nextBillingHeight = trialEndsHeight
	} else {
		billingCycle = 1
	}

	subAcc.Amount -= fee + firstPayment
	feePool.Amount += fee
	plan.ActiveSubs++

	subCounter.Count++
	subscription := &Subscription{
		Id:                subCounter.Count,
		PlanId:            msg.PlanId,
		SubscriberAddress: msg.SubscriberAddress,
		PlanCreator:       plan.CreatorAddress,
		StartedHeight:     c.currentHeight,
		NextBillingHeight: nextBillingHeight,
		TrialEndsHeight:   trialEndsHeight,
		Status:            status,
		BillingCycle:      billingCycle,
		LastBilledHeight:  c.currentHeight,
	}

	// Marshal all
	subBytes, err := Marshal(subscription)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	subCounterBytes, err := Marshal(subCounter)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	planBytes, err := Marshal(plan)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	subAccBytes, err := Marshal(subAcc)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	feeBytes, err := Marshal(feePool)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	sets := []*PluginSetOp{
		{Key: KeyForSubscription(subscription.Id), Value: subBytes},
		{Key: KeyForSubCounter(), Value: subCounterBytes},
		{Key: KeyForPlan(plan.Id), Value: planBytes},
		{Key: KeyForAccount(msg.SubscriberAddress), Value: subAccBytes},
		{Key: KeyForFeePool(c.Config.ChainId), Value: feeBytes},
	}

	// If first period was charged, credit the creator
	if firstPayment > 0 {
		creatorQId := rand.Uint64()
		creatorResp, creatorErr := c.plugin.StateRead(c, &PluginStateReadRequest{
			Keys: []*PluginKeyRead{
				{QueryId: creatorQId, Key: KeyForAccount(plan.CreatorAddress)},
			},
		})
		if creatorErr != nil {
			return &PluginDeliverResponse{Error: creatorErr}
		}
		if creatorResp.Error != nil {
			return &PluginDeliverResponse{Error: creatorResp.Error}
		}
		creatorAcc := new(Account)
		for _, r := range creatorResp.Results {
			if r.QueryId == creatorQId && len(r.Entries) > 0 {
				if err = Unmarshal(r.Entries[0].Value, creatorAcc); err != nil {
					return &PluginDeliverResponse{Error: err}
				}
			}
		}
		creatorAcc.Amount += firstPayment
		creatorAcc.Address = plan.CreatorAddress
		creatorBytes, err := Marshal(creatorAcc)
		if err != nil {
			return &PluginDeliverResponse{Error: err}
		}
		sets = append(sets, &PluginSetOp{Key: KeyForAccount(plan.CreatorAddress), Value: creatorBytes})
	}

	writeResp, err := c.plugin.StateWrite(c, &PluginStateWriteRequest{Sets: sets})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if writeResp.Error != nil {
		return &PluginDeliverResponse{Error: writeResp.Error}
	}
	log.Printf("Subscribe success: subId=%d planId=%d status=%s", subscription.Id, msg.PlanId, status)
	return &PluginDeliverResponse{}
}

// ─── DeliverTx: ProcessBilling ────────────────────────────────────────────────

func (c *Contract) DeliverProcessBilling(msg *MessageProcessBilling, fee uint64) *PluginDeliverResponse {
	log.Printf("DeliverProcessBilling: processor=%x subId=%d fee=%d", msg.ProcessorAddress, msg.SubscriptionId, fee)

	var (
		subQId       = rand.Uint64()
		processorQId = rand.Uint64()
		feeQId       = rand.Uint64()
		sub          = new(Subscription)
		processorAcc = new(Account)
		feePool      = new(Pool)
	)

	resp, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: subQId, Key: KeyForSubscription(msg.SubscriptionId)},
			{QueryId: processorQId, Key: KeyForAccount(msg.ProcessorAddress)},
			{QueryId: feeQId, Key: KeyForFeePool(c.Config.ChainId)},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if resp.Error != nil {
		return &PluginDeliverResponse{Error: resp.Error}
	}

	for _, r := range resp.Results {
		if len(r.Entries) == 0 {
			continue
		}
		switch r.QueryId {
		case subQId:
			if err = Unmarshal(r.Entries[0].Value, sub); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		case processorQId:
			if err = Unmarshal(r.Entries[0].Value, processorAcc); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		case feeQId:
			if err = Unmarshal(r.Entries[0].Value, feePool); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		}
	}

	if sub.Id == 0 {
		return &PluginDeliverResponse{Error: ErrInvalidAmount()}
	}
	if sub.Status == "cancelled" || sub.Status == "paused" {
		return &PluginDeliverResponse{Error: ErrInvalidAmount()}
	}
	if c.currentHeight < sub.NextBillingHeight {
		return &PluginDeliverResponse{Error: ErrInvalidAmount()}
	}
	if processorAcc.Amount < fee {
		return &PluginDeliverResponse{Error: ErrInsufficientFunds()}
	}

	// Read subscriber account and plan
	subAccQId := rand.Uint64()
	planQId := rand.Uint64()
	resp2, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: subAccQId, Key: KeyForAccount(sub.SubscriberAddress)},
			{QueryId: planQId, Key: KeyForPlan(sub.PlanId)},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if resp2.Error != nil {
		return &PluginDeliverResponse{Error: resp2.Error}
	}

	subAcc := new(Account)
	plan := new(Plan)
	for _, r := range resp2.Results {
		if len(r.Entries) == 0 {
			continue
		}
		switch r.QueryId {
		case subAccQId:
			if err = Unmarshal(r.Entries[0].Value, subAcc); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		case planQId:
			if err = Unmarshal(r.Entries[0].Value, plan); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		}
	}

	// Transition trial → active if trial window has passed
	if sub.Status == "trial" && c.currentHeight >= sub.TrialEndsHeight {
		sub.Status = "active"
	}

	sets := []*PluginSetOp{}

	if subAcc.Amount >= plan.Price {
		// Successful billing — deduct from subscriber
		subAcc.Amount -= plan.Price

		// Credit creator
		creatorQId := rand.Uint64()
		creatorResp, creatorErr := c.plugin.StateRead(c, &PluginStateReadRequest{
			Keys: []*PluginKeyRead{{QueryId: creatorQId, Key: KeyForAccount(plan.CreatorAddress)}},
		})
		if creatorErr != nil {
			return &PluginDeliverResponse{Error: creatorErr}
		}
		if creatorResp.Error != nil {
			return &PluginDeliverResponse{Error: creatorResp.Error}
		}
		creatorAcc := new(Account)
		for _, r := range creatorResp.Results {
			if r.QueryId == creatorQId && len(r.Entries) > 0 {
				if err = Unmarshal(r.Entries[0].Value, creatorAcc); err != nil {
					return &PluginDeliverResponse{Error: err}
				}
			}
		}
		creatorAcc.Amount += plan.Price
		creatorAcc.Address = plan.CreatorAddress
		creatorBytes, err := Marshal(creatorAcc)
		if err != nil {
			return &PluginDeliverResponse{Error: err}
		}
		sets = append(sets, &PluginSetOp{Key: KeyForAccount(plan.CreatorAddress), Value: creatorBytes})

		sub.BillingCycle++
		sub.LastBilledHeight = c.currentHeight
		sub.NextBillingHeight = c.currentHeight + (plan.IntervalDays * BlocksPerDay)

		// Billing reward to processor
		processorAcc.Amount += BillingReward
		log.Printf("Billing success: subId=%d cycle=%d reward=%d", sub.Id, sub.BillingCycle, BillingReward)
	} else {
		// Payment failure
		sub.Status = "failed"
		log.Printf("Billing failed: subId=%d insufficient subscriber funds", sub.Id)
	}

	processorAcc.Amount -= fee
	feePool.Amount += fee

	subBytes, err := Marshal(sub)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	subAccBytes, err := Marshal(subAcc)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	processorBytes, err := Marshal(processorAcc)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	feeBytes, err := Marshal(feePool)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	sets = append(sets,
		&PluginSetOp{Key: KeyForSubscription(sub.Id), Value: subBytes},
		&PluginSetOp{Key: KeyForAccount(sub.SubscriberAddress), Value: subAccBytes},
		&PluginSetOp{Key: KeyForAccount(msg.ProcessorAddress), Value: processorBytes},
		&PluginSetOp{Key: KeyForFeePool(c.Config.ChainId), Value: feeBytes},
	)

	writeResp, err := c.plugin.StateWrite(c, &PluginStateWriteRequest{Sets: sets})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if writeResp.Error != nil {
		return &PluginDeliverResponse{Error: writeResp.Error}
	}
	return &PluginDeliverResponse{}
}

// ─── DeliverTx: CancelSubscription ───────────────────────────────────────────

func (c *Contract) DeliverCancelSubscription(msg *MessageCancelSubscription, fee uint64) *PluginDeliverResponse {
	log.Printf("DeliverCancelSubscription: subscriber=%x subId=%d", msg.SubscriberAddress, msg.SubscriptionId)

	var (
		subQId    = rand.Uint64()
		subAccQId = rand.Uint64()
		feeQId    = rand.Uint64()
		sub       = new(Subscription)
		subAcc    = new(Account)
		feePool   = new(Pool)
	)

	resp, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: subQId, Key: KeyForSubscription(msg.SubscriptionId)},
			{QueryId: subAccQId, Key: KeyForAccount(msg.SubscriberAddress)},
			{QueryId: feeQId, Key: KeyForFeePool(c.Config.ChainId)},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if resp.Error != nil {
		return &PluginDeliverResponse{Error: resp.Error}
	}

	for _, r := range resp.Results {
		if len(r.Entries) == 0 {
			continue
		}
		switch r.QueryId {
		case subQId:
			if err = Unmarshal(r.Entries[0].Value, sub); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		case subAccQId:
			if err = Unmarshal(r.Entries[0].Value, subAcc); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		case feeQId:
			if err = Unmarshal(r.Entries[0].Value, feePool); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		}
	}

	if sub.Id == 0 {
		return &PluginDeliverResponse{Error: ErrInvalidAmount()}
	}
	if !bytes.Equal(sub.SubscriberAddress, msg.SubscriberAddress) {
		return &PluginDeliverResponse{Error: ErrInvalidAddress()}
	}
	if sub.Status == "cancelled" {
		return &PluginDeliverResponse{Error: ErrInvalidAmount()}
	}
	if subAcc.Amount < fee {
		return &PluginDeliverResponse{Error: ErrInsufficientFunds()}
	}

	sub.Status = "cancelled"
	subAcc.Amount -= fee
	feePool.Amount += fee

	// Decrement plan active count
	planQId := rand.Uint64()
	planResp, planErr := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{{QueryId: planQId, Key: KeyForPlan(sub.PlanId)}},
	})
	if planErr != nil {
		return &PluginDeliverResponse{Error: planErr}
	}
	if planResp.Error != nil {
		return &PluginDeliverResponse{Error: planResp.Error}
	}
	plan := new(Plan)
	for _, r := range planResp.Results {
		if r.QueryId == planQId && len(r.Entries) > 0 {
			if err = Unmarshal(r.Entries[0].Value, plan); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		}
	}
	if plan.ActiveSubs > 0 {
		plan.ActiveSubs--
	}

	subBytes, err := Marshal(sub)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	subAccBytes, err := Marshal(subAcc)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	feeBytes, err := Marshal(feePool)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	planBytes, err := Marshal(plan)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	writeResp, err := c.plugin.StateWrite(c, &PluginStateWriteRequest{
		Sets: []*PluginSetOp{
			{Key: KeyForSubscription(sub.Id), Value: subBytes},
			{Key: KeyForAccount(msg.SubscriberAddress), Value: subAccBytes},
			{Key: KeyForFeePool(c.Config.ChainId), Value: feeBytes},
			{Key: KeyForPlan(plan.Id), Value: planBytes},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if writeResp.Error != nil {
		return &PluginDeliverResponse{Error: writeResp.Error}
	}
	return &PluginDeliverResponse{}
}

// ─── DeliverTx: PauseSubscription ────────────────────────────────────────────

func (c *Contract) DeliverPauseSubscription(msg *MessagePauseSubscription, fee uint64) *PluginDeliverResponse {
	log.Printf("DeliverPauseSubscription: subscriber=%x subId=%d", msg.SubscriberAddress, msg.SubscriptionId)

	var (
		subQId    = rand.Uint64()
		subAccQId = rand.Uint64()
		feeQId    = rand.Uint64()
		sub       = new(Subscription)
		subAcc    = new(Account)
		feePool   = new(Pool)
	)

	resp, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: subQId, Key: KeyForSubscription(msg.SubscriptionId)},
			{QueryId: subAccQId, Key: KeyForAccount(msg.SubscriberAddress)},
			{QueryId: feeQId, Key: KeyForFeePool(c.Config.ChainId)},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if resp.Error != nil {
		return &PluginDeliverResponse{Error: resp.Error}
	}

	for _, r := range resp.Results {
		if len(r.Entries) == 0 {
			continue
		}
		switch r.QueryId {
		case subQId:
			if err = Unmarshal(r.Entries[0].Value, sub); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		case subAccQId:
			if err = Unmarshal(r.Entries[0].Value, subAcc); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		case feeQId:
			if err = Unmarshal(r.Entries[0].Value, feePool); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		}
	}

	if sub.Id == 0 {
		return &PluginDeliverResponse{Error: ErrInvalidAmount()}
	}
	if !bytes.Equal(sub.SubscriberAddress, msg.SubscriberAddress) {
		return &PluginDeliverResponse{Error: ErrInvalidAddress()}
	}
	if sub.Status != "active" && sub.Status != "trial" {
		return &PluginDeliverResponse{Error: ErrInvalidAmount()}
	}
	if subAcc.Amount < fee {
		return &PluginDeliverResponse{Error: ErrInsufficientFunds()}
	}

	sub.Status = "paused"
	subAcc.Amount -= fee
	feePool.Amount += fee

	subBytes, err := Marshal(sub)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	subAccBytes, err := Marshal(subAcc)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	feeBytes, err := Marshal(feePool)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	writeResp, err := c.plugin.StateWrite(c, &PluginStateWriteRequest{
		Sets: []*PluginSetOp{
			{Key: KeyForSubscription(sub.Id), Value: subBytes},
			{Key: KeyForAccount(msg.SubscriberAddress), Value: subAccBytes},
			{Key: KeyForFeePool(c.Config.ChainId), Value: feeBytes},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if writeResp.Error != nil {
		return &PluginDeliverResponse{Error: writeResp.Error}
	}
	return &PluginDeliverResponse{}
}

// ─── DeliverTx: ResumeSubscription ───────────────────────────────────────────

func (c *Contract) DeliverResumeSubscription(msg *MessageResumeSubscription, fee uint64) *PluginDeliverResponse {
	log.Printf("DeliverResumeSubscription: subscriber=%x subId=%d", msg.SubscriberAddress, msg.SubscriptionId)

	var (
		subQId    = rand.Uint64()
		subAccQId = rand.Uint64()
		feeQId    = rand.Uint64()
		sub       = new(Subscription)
		subAcc    = new(Account)
		feePool   = new(Pool)
	)

	resp, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: subQId, Key: KeyForSubscription(msg.SubscriptionId)},
			{QueryId: subAccQId, Key: KeyForAccount(msg.SubscriberAddress)},
			{QueryId: feeQId, Key: KeyForFeePool(c.Config.ChainId)},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if resp.Error != nil {
		return &PluginDeliverResponse{Error: resp.Error}
	}

	for _, r := range resp.Results {
		if len(r.Entries) == 0 {
			continue
		}
		switch r.QueryId {
		case subQId:
			if err = Unmarshal(r.Entries[0].Value, sub); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		case subAccQId:
			if err = Unmarshal(r.Entries[0].Value, subAcc); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		case feeQId:
			if err = Unmarshal(r.Entries[0].Value, feePool); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		}
	}

	if sub.Id == 0 {
		return &PluginDeliverResponse{Error: ErrInvalidAmount()}
	}
	if !bytes.Equal(sub.SubscriberAddress, msg.SubscriberAddress) {
		return &PluginDeliverResponse{Error: ErrInvalidAddress()}
	}
	if sub.Status != "paused" {
		return &PluginDeliverResponse{Error: ErrInvalidAmount()}
	}
	if subAcc.Amount < fee {
		return &PluginDeliverResponse{Error: ErrInsufficientFunds()}
	}

	// Read plan to recalculate billing interval from now
	planQId := rand.Uint64()
	planResp, planErr := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{{QueryId: planQId, Key: KeyForPlan(sub.PlanId)}},
	})
	if planErr != nil {
		return &PluginDeliverResponse{Error: planErr}
	}
	if planResp.Error != nil {
		return &PluginDeliverResponse{Error: planResp.Error}
	}
	plan := new(Plan)
	for _, r := range planResp.Results {
		if r.QueryId == planQId && len(r.Entries) > 0 {
			if err = Unmarshal(r.Entries[0].Value, plan); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		}
	}

	sub.Status = "active"
	sub.NextBillingHeight = c.currentHeight + (plan.IntervalDays * BlocksPerDay)
	subAcc.Amount -= fee
	feePool.Amount += fee

	subBytes, err := Marshal(sub)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	subAccBytes, err := Marshal(subAcc)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	feeBytes, err := Marshal(feePool)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	writeResp, err := c.plugin.StateWrite(c, &PluginStateWriteRequest{
		Sets: []*PluginSetOp{
			{Key: KeyForSubscription(sub.Id), Value: subBytes},
			{Key: KeyForAccount(msg.SubscriberAddress), Value: subAccBytes},
			{Key: KeyForFeePool(c.Config.ChainId), Value: feeBytes},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if writeResp.Error != nil {
		return &PluginDeliverResponse{Error: writeResp.Error}
	}
	return &PluginDeliverResponse{}
}

// ─── DeliverTx: UpdatePlan ────────────────────────────────────────────────────

func (c *Contract) DeliverUpdatePlan(msg *MessageUpdatePlan, fee uint64) *PluginDeliverResponse {
	log.Printf("DeliverUpdatePlan: creator=%x planId=%d fee=%d", msg.CreatorAddress, msg.PlanId, fee)

	var (
		planQId       = rand.Uint64()
		creatorAccQId = rand.Uint64()
		feeQId        = rand.Uint64()
		plan          = new(Plan)
		creatorAcc    = new(Account)
		feePool       = new(Pool)
	)

	resp, err := c.plugin.StateRead(c, &PluginStateReadRequest{
		Keys: []*PluginKeyRead{
			{QueryId: planQId, Key: KeyForPlan(msg.PlanId)},
			{QueryId: creatorAccQId, Key: KeyForAccount(msg.CreatorAddress)},
			{QueryId: feeQId, Key: KeyForFeePool(c.Config.ChainId)},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if resp.Error != nil {
		return &PluginDeliverResponse{Error: resp.Error}
	}

	for _, r := range resp.Results {
		if len(r.Entries) == 0 {
			continue
		}
		switch r.QueryId {
		case planQId:
			if err = Unmarshal(r.Entries[0].Value, plan); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		case creatorAccQId:
			if err = Unmarshal(r.Entries[0].Value, creatorAcc); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		case feeQId:
			if err = Unmarshal(r.Entries[0].Value, feePool); err != nil {
				return &PluginDeliverResponse{Error: err}
			}
		}
	}

	if plan.Id == 0 {
		return &PluginDeliverResponse{Error: ErrInvalidAmount()}
	}
	if !bytes.Equal(plan.CreatorAddress, msg.CreatorAddress) {
		return &PluginDeliverResponse{Error: ErrInvalidAddress()}
	}
	if creatorAcc.Amount < fee {
		return &PluginDeliverResponse{Error: ErrInsufficientFunds()}
	}

	// Apply non-zero / non-empty updates only
	if msg.Name != "" {
		plan.Name = msg.Name
	}
	if msg.Description != "" {
		plan.Description = msg.Description
	}
	if msg.NewPrice > 0 {
		plan.Price = msg.NewPrice
	}
	if msg.MaxSubscribers > 0 {
		plan.MaxSubscribers = msg.MaxSubscribers
	}

	creatorAcc.Amount -= fee
	feePool.Amount += fee

	planBytes, err := Marshal(plan)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	creatorBytes, err := Marshal(creatorAcc)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	feeBytes, err := Marshal(feePool)
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}

	writeResp, err := c.plugin.StateWrite(c, &PluginStateWriteRequest{
		Sets: []*PluginSetOp{
			{Key: KeyForPlan(plan.Id), Value: planBytes},
			{Key: KeyForAccount(msg.CreatorAddress), Value: creatorBytes},
			{Key: KeyForFeePool(c.Config.ChainId), Value: feeBytes},
		},
	})
	if err != nil {
		return &PluginDeliverResponse{Error: err}
	}
	if writeResp.Error != nil {
		return &PluginDeliverResponse{Error: writeResp.Error}
	}
	log.Printf("UpdatePlan success: planId=%d", plan.Id)
	return &PluginDeliverResponse{}
}

// ─── Lifecycle: EndBlock ──────────────────────────────────────────────────────

func (c *Contract) EndBlock(_ *PluginEndRequest) *PluginEndResponse {
	return &PluginEndResponse{}
}
