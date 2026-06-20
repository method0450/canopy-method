package main

import (
"bytes"
cryptorand "crypto/rand"
"encoding/base64"
"encoding/hex"
"encoding/json"
"fmt"
"io"
"net/http"
"testing"
"time"

"github.com/canopy-network/go-plugin/contract"
"github.com/canopy-network/go-plugin/crypto"
"google.golang.org/protobuf/proto"
"google.golang.org/protobuf/types/known/anypb"
)

const (
tsQueryURL      = "http://localhost:50002"
tsAdminURL      = "http://localhost:50003"
tsNetworkID     = uint64(1)
tsChainID       = uint64(1)
tsPassword      = "testpassword123"
tsValidatorAddr = "e7c7dad131a03f7ea0cc09a637ad096eb3495f77"
tsValidatorPass = "test123"
)

func tsValidatorKey() (*tsKeyGroup, error) {
b, _ := json.Marshal(map[string]string{"address": tsValidatorAddr, "password": tsValidatorPass})
resp, err := tsPost(tsAdminURL+"/v1/admin/keystore-get", b)
if err != nil {
return nil, err
}
var kg tsKeyGroup
json.Unmarshal(resp, &kg)
return &kg, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

type tsKeyGroup struct {
Address    string `json:"address"`
PublicKey  string `json:"publicKey"`
PrivateKey string `json:"privateKey"`
}

func tsPost(url string, body []byte) ([]byte, error) {
resp, err := http.Post(url, "application/json", bytes.NewReader(body))
if err != nil {
return nil, err
}
defer resp.Body.Close()
return io.ReadAll(resp.Body)
}

func tsNewKey(nickname string) (string, error) {
b, _ := json.Marshal(map[string]string{"nickname": nickname, "password": tsPassword})
resp, err := tsPost(tsAdminURL+"/v1/admin/keystore-new-key", b)
if err != nil {
return "", err
}
var addr string
json.Unmarshal(resp, &addr)
return addr, nil
}

func tsGetKey(addr string) (*tsKeyGroup, error) {
b, _ := json.Marshal(map[string]string{"address": addr, "password": tsPassword})
resp, err := tsPost(tsAdminURL+"/v1/admin/keystore-get", b)
if err != nil {
return nil, err
}
var kg tsKeyGroup
json.Unmarshal(resp, &kg)
return &kg, nil
}

func tsGetHeight() (uint64, error) {
resp, err := tsPost(tsQueryURL+"/v1/query/height", []byte("{}"))
if err != nil {
return 0, err
}
var r struct {
Height uint64 `json:"height"`
}
json.Unmarshal(resp, &r)
return r.Height, nil
}

func tsGetBalance(addr string) (uint64, error) {
b, _ := json.Marshal(map[string]string{"address": addr})
resp, err := tsPost(tsQueryURL+"/v1/query/account", b)
if err != nil {
return 0, err
}
var r struct {
Amount uint64 `json:"amount"`
}
json.Unmarshal(resp, &r)
return r.Amount, nil
}

func tsH2B(h string) []byte {
b, _ := hex.DecodeString(h)
return b
}

func tsH2B64(h string) string {
return base64.StdEncoding.EncodeToString(tsH2B(h))
}

func tsWaitConfirm(addr, txHash string, t *testing.T) error {
deadline := time.Now().Add(60 * time.Second)
for time.Now().Before(deadline) {
b, _ := json.Marshal(map[string]string{"hash": txHash})
resp, _ := tsPost(tsQueryURL+"/v1/query/tx-by-hash", b)
if len(resp) > 2 && string(resp) != "{}" {
// non-empty response means tx landed
return nil
}
time.Sleep(time.Second)
}
return fmt.Errorf("tx %s not confirmed within 60s", txHash)
}

func tsSendTx(kg *tsKeyGroup, msgType, typeURL string, msgProto proto.Message, fee uint64) (string, error) {
height, err := tsGetHeight()
if err != nil {
return "", err
}
txTime := uint64(time.Now().UnixMicro())

// Marshal proto message for signing
msgBytes, err := proto.Marshal(msgProto)
if err != nil {
return "", fmt.Errorf("marshal msg: %v", err)
}
anyMsg := &anypb.Any{TypeUrl: typeURL, Value: msgBytes}

signBytes, err := crypto.GetSignBytes(msgType, anyMsg, txTime, height, fee, "", tsNetworkID, tsChainID)
if err != nil {
return "", fmt.Errorf("GetSignBytes: %v", err)
}

privKey, err := crypto.StringToBLS12381PrivateKey(kg.PrivateKey)
if err != nil {
return "", fmt.Errorf("privkey: %v", err)
}
sig := privKey.Sign(signBytes)

// Build JSON tx — use msgBytes/typeURL for plugin messages
tx := map[string]interface{}{
"type":       msgType,
"msgTypeUrl": typeURL,
"msgBytes":   hex.EncodeToString(msgBytes),
"signature": map[string]string{
"publicKey": kg.PublicKey,
"signature": hex.EncodeToString(sig),
},
"time":          txTime,
"createdHeight": height,
"fee":           fee,
"memo":          "",
"networkID":     tsNetworkID,
"chainID":       tsChainID,
}

body, _ := json.Marshal(tx)
resp, err := tsPost(tsQueryURL+"/v1/tx", body)
if err != nil {
return "", err
}

var hash string
if err := json.Unmarshal(resp, &hash); err != nil {
return "", fmt.Errorf("parse response: %v body: %s", err, string(resp))
}
if hash == "" {
return "", fmt.Errorf("empty hash, response: %s", string(resp))
}
return hash, nil
}

// tsFaucet sends plugin MessageSend from the validator account to fund a test account
func tsFaucet(kg *tsKeyGroup, recipientAddr string, amount uint64) (string, error) {
height, err := tsGetHeight()
if err != nil {
return "", err
}
txTime := uint64(time.Now().UnixMicro())

msgProto := &contract.MessageSend{
FromAddress: tsH2B(kg.Address),
ToAddress:   tsH2B(recipientAddr),
Amount:      amount,
}
msgBytes, _ := proto.Marshal(msgProto)
anyMsg := &anypb.Any{TypeUrl: "type.googleapis.com/types.MessageSend", Value: msgBytes}

signBytes, err := crypto.GetSignBytes("send", anyMsg, txTime, height, 10000, "", tsNetworkID, tsChainID)
if err != nil {
return "", fmt.Errorf("GetSignBytes: %v", err)
}
privKey, _ := crypto.StringToBLS12381PrivateKey(kg.PrivateKey)
sig := privKey.Sign(signBytes)

tx := map[string]interface{}{
"type": "send",
"msg": map[string]interface{}{
"fromAddress": base64.StdEncoding.EncodeToString(tsH2B(kg.Address)),
"toAddress":   base64.StdEncoding.EncodeToString(tsH2B(recipientAddr)),
"amount":      amount,
},
"signature": map[string]string{
"publicKey": kg.PublicKey,
"signature": hex.EncodeToString(sig),
},
"time":          txTime,
"createdHeight": height,
"fee":           uint64(10000),
"memo":          "",
"networkID":     tsNetworkID,
"chainID":       tsChainID,
}

body, _ := json.Marshal(tx)
resp, err := tsPost(tsQueryURL+"/v1/tx", body)
if err != nil {
return "", err
}
var hash string
if err := json.Unmarshal(resp, &hash); err != nil {
return "", fmt.Errorf("faucet parse: %v body: %s", err, string(resp))
}
return hash, nil
}

func tsSuffix() string {
b := make([]byte, 4)
cryptorand.Read(b)
return hex.EncodeToString(b)
}

// ── tests ─────────────────────────────────────────────────────────────────────

func TestTrustStakeBasic(t *testing.T) {
sfx := tsSuffix()
stakerAddr, err := tsNewKey("ts_staker_" + sfx)
if err != nil {
t.Fatalf("create staker: %v", err)
}
targetAddr, err := tsNewKey("ts_target_" + sfx)
if err != nil {
t.Fatalf("create target: %v", err)
}
t.Logf("staker=%s target=%s", stakerAddr, targetAddr)

stakerKey, _ := tsGetKey(stakerAddr)
valKey, err := tsValidatorKey()
if err != nil {
t.Fatalf("validator key: %v", err)
}

// Faucet staker from validator
hash, err := tsFaucet(valKey, stakerAddr, 1_000_000_000)
if err != nil {
t.Fatalf("faucet: %v", err)
}
if err := tsWaitConfirm(stakerAddr, hash, t); err != nil {
t.Fatalf("faucet confirm: %v", err)
}

balBefore, _ := tsGetBalance(stakerAddr)
t.Logf("balance before stake: %d", balBefore)

// stake_trust
stakeAmount := uint64(100_000_000)
hash, err = tsSendTx(stakerKey, "stake_trust", "type.googleapis.com/types.MessageStakeTrust",
&contract.MessageStakeTrust{
StakerAddress: tsH2B(stakerAddr),
TargetAddress: tsH2B(targetAddr),
Amount:        stakeAmount,
}, 10000)
if err != nil {
t.Fatalf("stake_trust: %v", err)
}
if err := tsWaitConfirm(stakerAddr, hash, t); err != nil {
t.Fatalf("stake_trust confirm: %v", err)
}
t.Logf("stake_trust confirmed: %s", hash)

balAfter, _ := tsGetBalance(stakerAddr)
t.Logf("balance after stake: %d", balAfter)

expectedDelta := stakeAmount + 10000 // amount + fee
if balBefore-balAfter != expectedDelta {
t.Errorf("expected balance decrease of %d, got %d", expectedDelta, balBefore-balAfter)
}
}

func TestTrustStakeAndWithdraw(t *testing.T) {
sfx := tsSuffix()
stakerAddr, _ := tsNewKey("ts_wd_" + sfx)
targetAddr, _ := tsNewKey("ts_wdt_" + sfx)
stakerKey, _ := tsGetKey(stakerAddr)
valKey, _ := tsValidatorKey()

hash, err := tsFaucet(valKey, stakerAddr, 1_000_000_000)
if err != nil {
t.Fatalf("faucet: %v", err)
}
tsWaitConfirm(stakerAddr, hash, t)

// Stake
hash, err = tsSendTx(stakerKey, "stake_trust", "type.googleapis.com/types.MessageStakeTrust",
&contract.MessageStakeTrust{
StakerAddress: tsH2B(stakerAddr),
TargetAddress: tsH2B(targetAddr),
Amount:        50_000_000,
}, 10000)
if err != nil {
t.Fatalf("stake: %v", err)
}
tsWaitConfirm(stakerAddr, hash, t)
t.Logf("staked: %s", hash)

// Initiate withdraw
hash, err = tsSendTx(stakerKey, "withdraw_stake", "type.googleapis.com/types.MessageWithdrawStake",
&contract.MessageWithdrawStake{
StakerAddress: tsH2B(stakerAddr),
TargetAddress: tsH2B(targetAddr),
}, 10000)
if err != nil {
t.Fatalf("withdraw initiate: %v", err)
}
tsWaitConfirm(stakerAddr, hash, t)
t.Logf("withdraw initiated: %s", hash)

// Immediately try to claim (should fail — cooldown = 5 blocks)
hash2, err := tsSendTx(stakerKey, "withdraw_stake", "type.googleapis.com/types.MessageWithdrawStake",
&contract.MessageWithdrawStake{
StakerAddress: tsH2B(stakerAddr),
TargetAddress: tsH2B(targetAddr),
}, 10000)
if err != nil {
t.Logf("early claim rejected at submission (expected): %v", err)
} else {
// may land in failed-txs
time.Sleep(3 * time.Second)
b, _ := json.Marshal(map[string]interface{}{"address": stakerAddr, "perPage": 5})
resp, _ := tsPost(tsQueryURL+"/v1/query/failed-txs", b)
t.Logf("early claim hash=%s failed-txs response: %s", hash2, string(resp))
}
}

func TestTrustStakeProposeSlash(t *testing.T) {
sfx := tsSuffix()
stakerAddr, _ := tsNewKey("ts_ps_staker_" + sfx)
targetAddr, _ := tsNewKey("ts_ps_target_" + sfx)
stakerKey, _ := tsGetKey(stakerAddr)
valKey, _ := tsValidatorKey()

hash, err := tsFaucet(valKey, stakerAddr, 1_000_000_000)
if err != nil {
t.Fatalf("faucet: %v", err)
}
tsWaitConfirm(stakerAddr, hash, t)

// Must stake first to be eligible to propose
hash, err = tsSendTx(stakerKey, "stake_trust", "type.googleapis.com/types.MessageStakeTrust",
&contract.MessageStakeTrust{
StakerAddress: tsH2B(stakerAddr),
TargetAddress: tsH2B(targetAddr),
Amount:        100_000_000,
}, 10000)
if err != nil {
t.Fatalf("stake: %v", err)
}
tsWaitConfirm(stakerAddr, hash, t)

// Wait extra blocks for state to settle
time.Sleep(6 * time.Second)

// Propose slash
hash, err = tsSendTx(stakerKey, "propose_slash", "type.googleapis.com/types.MessageProposeSlash",
&contract.MessageProposeSlash{
InitiatorAddress: tsH2B(stakerAddr),
TargetAddress:    tsH2B(targetAddr),
Reason:           "test: target misbehaved",
}, 10000)
if err != nil {
t.Fatalf("propose_slash: %v", err)
}
if err := tsWaitConfirm(stakerAddr, hash, t); err != nil {
t.Fatalf("propose_slash confirm: %v", err)
}
t.Logf("propose_slash confirmed: %s", hash)
}
