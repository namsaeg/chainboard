// Package params holds the consensus parameters of a chainboard network.
//
// Every value here is part of consensus: two nodes with different params are on
// different networks. Units:
//   - coin amounts are in "units"; 1 coin = Coin units
//   - storage prices are in units per raw GB-year (1 GB = 1e9 bytes)
//   - heights and durations are in blocks
package params

import (
	"math/big"
	"time"
)

const (
	Version = "0.3.1"

	Coin = 100_000_000 // units per coin

	ChunkSize    = 256 * 1024 // bytes per chunk
	SubChunkSize = 8 * 1024   // bytes per sub-chunk (proofs carry one sub-chunk)

	GB = 1_000_000_000

	TierPermanent uint16 = 0xFFFF // retention tier code for permanent storage
	TierFree      uint16 = 1      // the only tier of anonymous (free) posts: one month

	MaxPostBytes   = 1024 // text of one post, UTF-8 bytes
	MaxBoardBytes  = 32
	MaxAttachments = 4
	MaxTextBundles = 256 // text bundles one seal block may place
)

// Params is one network's consensus configuration.
type Params struct {
	Name  string
	Magic uint32 // network id carried in every p2p handshake

	BlockTime time.Duration

	// VDF and mining.
	VDFIterations     uint64 // SHA-256 iterations per VDF step (testnet: ~43 ms with SHA-NI)
	ChallengesPerStep int    // challenges unlocked per stored partition per VDF step
	PartitionChunks   uint64 // chunks per partition
	PackMemoryKiB     uint32 // Argon2id memory per packing seed (packing cost)
	PackTime          uint32 // Argon2id passes
	GenesisTarget     *big.Int
	MaxTarget         *big.Int
	RetargetWindow    int // blocks used for difficulty retarget
	GenesisDataChunks uint64

	// Time units in blocks.
	BlocksPerDay   uint64
	BlocksPerYear  uint64
	BlocksPerMonth uint64

	// Issuance.
	BootstrapReward uint64 // units per block once the ramp has ended
	RampBlocks      uint64 // linear ramp from 0 to BootstrapReward
	HalvingBlocks   uint64
	TailRatePPM     uint64 // tail issuance, parts per million of supply per year
	CapacityTarget  uint64 // target proven/needed storage ratio, in permille (1500 = 1.5)

	// Rewards.
	LiquidPermille   uint64 // share of income paid out immediately
	VestingBlocks    uint64
	ProofEpoch       uint64 // blocks per storage-proof epoch
	ForfeitEpochs    uint64 // missed epochs before unvested income is forfeited
	RepairReleasePPM uint64 // share of the repair pool released per day, ppm

	// Storage pricing.
	InitialPrice   uint64 // units per raw GB-year at genesis
	PriceEMABlocks uint64 // smoothing window (30 days)
	PriceMaxDayPPM uint64 // max price change per day, ppm (50_000 = 5%)
	TextReplicas   uint64
	MediaReplicas  uint64
	PermYears      uint64 // permanent tier is charged this many years up front

	// Text lanes.
	TextBytesPerBlock uint64
	FreeLanePermille  uint64 // share of text capacity for the anonymous (RLN) lane
	BaseFeeInit       uint64 // units per byte, fee lane
	BaseFeeMin        uint64
	StakeAgingBlocks  uint64
	UnbondBlocks      uint64
	DupWindowBlocks   uint64
	DupMinBytes       uint64 // shorter texts are not counted as repeats

	// Burst surcharge. Each account has a score that rises by one per signed
	// transaction and decays by a factor e every BurstTauBlocks, so a steady
	// rate of r per BurstTauBlocks settles near r. Above BurstFree the base
	// fee doubles every BurstStep points.
	BurstTauBlocks uint64
	BurstFree      uint64
	BurstStep      uint64

	// Text storage. Post bodies are not in blocks: transactions carry their
	// hash, and every TextSealBlocks the texts of that window are bundled per
	// retention bucket into the weave like uploads.
	TextSealBlocks uint64

	// Anonymous lane (RLN). A membership bonds stake for a per-epoch message
	// limit; posts prove membership without revealing which member wrote them.
	RLNEpochBlocks   uint64 // blocks per rate-limit epoch
	RLNMaxLimit      uint64 // most messages per epoch of one membership (≤ 65535)
	RLNBondPerMsg    uint64 // bond needed per message of limit
	RLNRootWindow    uint64 // blocks a replaced membership root stays usable in proofs
	RLNSlashPermille uint64 // share of a slashed bond paid to the block's miner; the rest is burned

	// Media.
	MinMediaPerBlock  uint64 // bytes; floor of the automatic media cap
	MediaHeadroomPerm uint64 // permille of proven storage usable for data (500 = 50%)
	FileCapDivisor    uint64 // file cap = daily media cap / this
}

func mustHex(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 16)
	if !ok {
		panic("bad hex " + s)
	}
	return v
}

// Testnet is the public test network.
func Testnet() *Params {
	p := &Params{
		Name:              "testnet",
		Magic:             0xCB0A0003, // 0.3.0: new genesis (texts moved off-chain)
		BlockTime:         60 * time.Second,
		VDFIterations:     400_000,
		ChallengesPerStep: 4,
		PartitionChunks:   4096, // 1 GiB
		PackMemoryKiB:     4096, // 4 MiB per 256 KiB chunk
		PackTime:          1,
		GenesisTarget:     mustHex("0400000000000000000000000000000000000000000000000000000000000000"),
		MaxTarget:         mustHex("7fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"),
		RetargetWindow:    30,
		GenesisDataChunks: 64,
	}
	p.setTime(60)
	p.setEconomics()
	return p
}

// Devnet is a fast local network for development and tests.
func Devnet() *Params {
	p := &Params{
		Name:              "devnet",
		Magic:             0xCB0D0003,
		BlockTime:         2 * time.Second,
		VDFIterations:     100_000,
		ChallengesPerStep: 4,
		PartitionChunks:   16,
		PackMemoryKiB:     64,
		PackTime:          1,
		GenesisTarget:     mustHex("0200000000000000000000000000000000000000000000000000000000000000"),
		MaxTarget:         mustHex("7fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"),
		RetargetWindow:    10,
		GenesisDataChunks: 32,
	}
	p.setTime(2)
	p.setEconomics()
	// Short time scales so every rule can be exercised in seconds.
	p.ProofEpoch = 30
	p.StakeAgingBlocks = 5
	p.UnbondBlocks = 10
	p.VestingBlocks = 50
	p.RampBlocks = 20
	p.DupWindowBlocks = 30
	p.RLNEpochBlocks = 30 // one minute
	p.TextSealBlocks = 10
	p.BurstTauBlocks = 30
	return p
}

func (p *Params) setTime(blockSeconds uint64) {
	p.BlocksPerDay = 86_400 / blockSeconds
	p.BlocksPerYear = 365 * p.BlocksPerDay
	p.BlocksPerMonth = 30 * p.BlocksPerDay
}

func (p *Params) setEconomics() {
	p.BootstrapReward = 50 * Coin
	p.RampBlocks = 182 * p.BlocksPerDay
	p.HalvingBlocks = 4 * p.BlocksPerYear
	p.TailRatePPM = 10_000 // 1% per year before feedback
	p.CapacityTarget = 1500
	p.LiquidPermille = 250
	p.VestingBlocks = 180 * p.BlocksPerDay
	p.ProofEpoch = p.BlocksPerDay
	p.ForfeitEpochs = 3
	p.RepairReleasePPM = 10_000 // 1% per day
	p.InitialPrice = 1 * Coin   // 1 coin per raw GB-year: "1 storage unit"
	p.PriceEMABlocks = 30 * p.BlocksPerDay
	p.PriceMaxDayPPM = 50_000
	p.TextReplicas = 20
	p.MediaReplicas = 6
	p.PermYears = 200
	p.TextBytesPerBlock = 256 * 1024
	p.FreeLanePermille = 300
	p.BaseFeeInit = 10
	p.BaseFeeMin = 1
	p.RLNEpochBlocks = p.BlocksPerDay / 24 // one hour
	p.RLNMaxLimit = 30                     // 30 posts an hour = 720 a day
	p.RLNBondPerMsg = Coin
	p.RLNRootWindow = 15 // covers the send delay (≤ 5 min) and propagation
	p.RLNSlashPermille = 500
	p.StakeAgingBlocks = 7 * p.BlocksPerDay
	p.UnbondBlocks = 30 * p.BlocksPerDay
	p.DupWindowBlocks = p.BlocksPerDay
	p.DupMinBytes = 32 // about ten Korean characters
	p.BurstTauBlocks = p.BlocksPerDay / 24
	p.BurstFree = 30 // the honest ceiling: 30 an hour
	p.BurstStep = 10
	p.TextSealBlocks = p.BlocksPerDay / 24 // hourly bundles
	p.MinMediaPerBlock = 4 * 1024 * 1024
	p.MediaHeadroomPerm = 500
	p.FileCapDivisor = 100
}

// TierMonths validates a retention tier and returns its length in months.
// Allowed: 1 month, whole years from 1 to 100, or permanent.
func TierMonths(t uint16) (months uint64, permanent bool, ok bool) {
	switch {
	case t == TierPermanent:
		return 0, true, true
	case t == 1:
		return 1, false, true
	case t >= 12 && t <= 1200 && t%12 == 0:
		return uint64(t), false, true
	}
	return 0, false, false
}

// RLNScope separates this network's nullifiers from every other network's.
func (p *Params) RLNScope() uint64 { return uint64(p.Magic) }

// TextCapFree and TextCapFee split the per-block text capacity between lanes.
func (p *Params) TextCapFree() uint64 { return p.TextBytesPerBlock * p.FreeLanePermille / 1000 }
func (p *Params) TextCapFee() uint64  { return p.TextBytesPerBlock - p.TextCapFree() }

// ByName returns the params for a network name.
func ByName(name string) *Params {
	switch name {
	case "devnet":
		return Devnet()
	case "testnet":
		return Testnet()
	}
	return nil
}
