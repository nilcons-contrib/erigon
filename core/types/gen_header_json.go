// Code generated by github.com/fjl/gencodec. DO NOT EDIT.

package types

import (
	"encoding/json"
	"errors"
	"math/big"

	"github.com/gballet/go-verkle"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/hexutil"
	"github.com/ledgerwatch/erigon-lib/common/hexutility"
)

var _ = (*headerMarshaling)(nil)

// MarshalJSON marshals as JSON.
func (h Header) MarshalJSON() ([]byte, error) {
	type Header struct {
		ParentHash            common.Hash      `json:"parentHash"       gencodec:"required"`
		UncleHash             common.Hash      `json:"sha3Uncles"       gencodec:"required"`
		Coinbase              common.Address   `json:"miner"`
		Root                  common.Hash      `json:"stateRoot"        gencodec:"required"`
		TxHash                common.Hash      `json:"transactionsRoot" gencodec:"required"`
		ReceiptHash           common.Hash      `json:"receiptsRoot"     gencodec:"required"`
		Bloom                 Bloom            `json:"logsBloom"        gencodec:"required"`
		Difficulty            *hexutil.Big     `json:"difficulty"       gencodec:"required"`
		Number                *hexutil.Big     `json:"number"           gencodec:"required"`
		GasLimit              hexutil.Uint64   `json:"gasLimit"         gencodec:"required"`
		GasUsed               hexutil.Uint64   `json:"gasUsed"          gencodec:"required"`
		Time                  hexutil.Uint64   `json:"timestamp"        gencodec:"required"`
		Extra                 hexutility.Bytes `json:"extraData"        gencodec:"required"`
		MixDigest             common.Hash      `json:"mixHash"`
		Nonce                 BlockNonce       `json:"nonce"`
		AuRaStep              uint64
		AuRaSeal              []byte
		BaseFee               *hexutil.Big    `json:"baseFeePerGas"`
		WithdrawalsHash       *common.Hash    `json:"withdrawalsRoot"`
		BlobGasUsed           *hexutil.Uint64 `json:"blobGasUsed"`
		ExcessBlobGas         *hexutil.Uint64 `json:"excessBlobGas"`
		ParentBeaconBlockRoot *common.Hash    `json:"parentBeaconBlockRoot"`
		Verkle                bool
		VerkleProof           []byte
		VerkleKeyVals         []verkle.KeyValuePair
		Hash                  common.Hash `json:"hash"`
	}
	var enc Header
	enc.ParentHash = h.ParentHash
	enc.UncleHash = h.UncleHash
	enc.Coinbase = h.Coinbase
	enc.Root = h.Root
	enc.TxHash = h.TxHash
	enc.ReceiptHash = h.ReceiptHash
	enc.Bloom = h.Bloom
	enc.Difficulty = (*hexutil.Big)(h.Difficulty)
	enc.Number = (*hexutil.Big)(h.Number)
	enc.GasLimit = hexutil.Uint64(h.GasLimit)
	enc.GasUsed = hexutil.Uint64(h.GasUsed)
	enc.Time = hexutil.Uint64(h.Time)
	enc.Extra = h.Extra
	enc.MixDigest = h.MixDigest
	enc.Nonce = h.Nonce
	enc.AuRaStep = h.AuRaStep
	enc.AuRaSeal = h.AuRaSeal
	enc.BaseFee = (*hexutil.Big)(h.BaseFee)
	enc.WithdrawalsHash = h.WithdrawalsHash
	enc.BlobGasUsed = (*hexutil.Uint64)(h.BlobGasUsed)
	enc.ExcessBlobGas = (*hexutil.Uint64)(h.ExcessBlobGas)
	enc.ParentBeaconBlockRoot = h.ParentBeaconBlockRoot
	enc.Verkle = h.Verkle
	enc.VerkleProof = h.VerkleProof
	enc.VerkleKeyVals = h.VerkleKeyVals
	enc.Hash = h.Hash()
	return json.Marshal(&enc)
}

// UnmarshalJSON unmarshals from JSON.
func (h *Header) UnmarshalJSON(input []byte) error {
	type Header struct {
		ParentHash            *common.Hash      `json:"parentHash"       gencodec:"required"`
		UncleHash             *common.Hash      `json:"sha3Uncles"       gencodec:"required"`
		Coinbase              *common.Address   `json:"miner"`
		Root                  *common.Hash      `json:"stateRoot"        gencodec:"required"`
		TxHash                *common.Hash      `json:"transactionsRoot" gencodec:"required"`
		ReceiptHash           *common.Hash      `json:"receiptsRoot"     gencodec:"required"`
		Bloom                 *Bloom            `json:"logsBloom"        gencodec:"required"`
		Difficulty            *hexutil.Big      `json:"difficulty"       gencodec:"required"`
		Number                *hexutil.Big      `json:"number"           gencodec:"required"`
		GasLimit              *hexutil.Uint64   `json:"gasLimit"         gencodec:"required"`
		GasUsed               *hexutil.Uint64   `json:"gasUsed"          gencodec:"required"`
		Time                  *hexutil.Uint64   `json:"timestamp"        gencodec:"required"`
		Extra                 *hexutility.Bytes `json:"extraData"        gencodec:"required"`
		MixDigest             *common.Hash      `json:"mixHash"`
		Nonce                 *BlockNonce       `json:"nonce"`
		AuRaStep              *uint64
		AuRaSeal              []byte
		BaseFee               *hexutil.Big    `json:"baseFeePerGas"`
		WithdrawalsHash       *common.Hash    `json:"withdrawalsRoot"`
		BlobGasUsed           *hexutil.Uint64 `json:"blobGasUsed"`
		ExcessBlobGas         *hexutil.Uint64 `json:"excessBlobGas"`
		ParentBeaconBlockRoot *common.Hash    `json:"parentBeaconBlockRoot"`
		Verkle                *bool
		VerkleProof           []byte
		VerkleKeyVals         []verkle.KeyValuePair
	}
	var dec Header
	if err := json.Unmarshal(input, &dec); err != nil {
		return err
	}
	if dec.ParentHash == nil {
		return errors.New("missing required field 'parentHash' for Header")
	}
	h.ParentHash = *dec.ParentHash
	if dec.UncleHash == nil {
		return errors.New("missing required field 'sha3Uncles' for Header")
	}
	h.UncleHash = *dec.UncleHash
	if dec.Coinbase != nil {
		h.Coinbase = *dec.Coinbase
	}
	if dec.Root == nil {
		return errors.New("missing required field 'stateRoot' for Header")
	}
	h.Root = *dec.Root
	if dec.TxHash == nil {
		return errors.New("missing required field 'transactionsRoot' for Header")
	}
	h.TxHash = *dec.TxHash
	if dec.ReceiptHash == nil {
		return errors.New("missing required field 'receiptsRoot' for Header")
	}
	h.ReceiptHash = *dec.ReceiptHash
	if dec.Bloom == nil {
		return errors.New("missing required field 'logsBloom' for Header")
	}
	h.Bloom = *dec.Bloom
	if dec.Difficulty == nil {
		return errors.New("missing required field 'difficulty' for Header")
	}
	h.Difficulty = (*big.Int)(dec.Difficulty)
	if dec.Number == nil {
		return errors.New("missing required field 'number' for Header")
	}
	h.Number = (*big.Int)(dec.Number)
	if dec.GasLimit == nil {
		return errors.New("missing required field 'gasLimit' for Header")
	}
	h.GasLimit = uint64(*dec.GasLimit)
	if dec.GasUsed == nil {
		return errors.New("missing required field 'gasUsed' for Header")
	}
	h.GasUsed = uint64(*dec.GasUsed)
	if dec.Time == nil {
		return errors.New("missing required field 'timestamp' for Header")
	}
	h.Time = uint64(*dec.Time)
	if dec.Extra == nil {
		return errors.New("missing required field 'extraData' for Header")
	}
	h.Extra = *dec.Extra
	if dec.MixDigest != nil {
		h.MixDigest = *dec.MixDigest
	}
	if dec.Nonce != nil {
		h.Nonce = *dec.Nonce
	}
	if dec.AuRaStep != nil {
		h.AuRaStep = *dec.AuRaStep
	}
	if dec.AuRaSeal != nil {
		h.AuRaSeal = dec.AuRaSeal
	}
	if dec.BaseFee != nil {
		h.BaseFee = (*big.Int)(dec.BaseFee)
	}
	if dec.WithdrawalsHash != nil {
		h.WithdrawalsHash = dec.WithdrawalsHash
	}
	if dec.BlobGasUsed != nil {
		h.BlobGasUsed = (*uint64)(dec.BlobGasUsed)
	}
	if dec.ExcessBlobGas != nil {
		h.ExcessBlobGas = (*uint64)(dec.ExcessBlobGas)
	}
	if dec.ParentBeaconBlockRoot != nil {
		h.ParentBeaconBlockRoot = dec.ParentBeaconBlockRoot
	}
	if dec.Verkle != nil {
		h.Verkle = *dec.Verkle
	}
	if dec.VerkleProof != nil {
		h.VerkleProof = dec.VerkleProof
	}
	if dec.VerkleKeyVals != nil {
		h.VerkleKeyVals = dec.VerkleKeyVals
	}
	return nil
}
