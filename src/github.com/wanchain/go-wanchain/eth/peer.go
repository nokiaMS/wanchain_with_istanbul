// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package eth

import (
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/wanchain/go-wanchain/common"
	"github.com/wanchain/go-wanchain/core/types"
	"github.com/wanchain/go-wanchain/p2p"
	"github.com/wanchain/go-wanchain/rlp"
	set "gopkg.in/fatih/set.v0"
)

var (
	errClosed            = errors.New("peer set is closed")
	errAlreadyRegistered = errors.New("peer is already registered")
	errNotRegistered     = errors.New("peer is not registered")
)

const (
	maxKnownTxs      = 32768 // Maximum transactions hashes to keep in the known list (prevent DOS)
	maxKnownBlocks   = 1024  // Maximum block hashes to keep in the known list (prevent DOS)
	handshakeTimeout = 5 * time.Second
)

// PeerInfo represents a short summary of the Ethereum sub-protocol metadata known
// about a connected peer.
type PeerInfo struct {
	Version    int      `json:"version"`    // Ethereum protocol version negotiated
	Difficulty *big.Int `json:"difficulty"` // Total difficulty of the peer's blockchain
	Head       string   `json:"head"`       // SHA3 hash of the peer's best owned block
}

//以太坊peer类型.
type peer struct {
	id string

	*p2p.Peer
	rw p2p.MsgReadWriter

	version  int         // Protocol version negotiated
	forkDrop *time.Timer // Timed connection dropper if forks aren't validated in time

	head common.Hash
	td   *big.Int
	lock sync.RWMutex

	knownTxs    *set.Set // Set of transaction hashes known to be known by this peer	//已经确认peer已经收到的交易.
	knownBlocks *set.Set // Set of block hashes known to be known by this peer		//本节点存储的peer已知的块列表.(在p2p.send之前块信息就被添加了,所以有可能peer其实不真正获得了这个块hash.)
}

func newPeer(version int, p *p2p.Peer, rw p2p.MsgReadWriter) *peer {
	id := p.ID()

	return &peer{
		Peer:        p,
		rw:          rw,
		version:     version,
		id:          fmt.Sprintf("%x", id[:8]),
		knownTxs:    set.New(),
		knownBlocks: set.New(),
	}
}

// Info gathers and returns a collection of metadata known about a peer.
func (p *peer) Info() *PeerInfo {
	hash, td := p.Head()

	return &PeerInfo{
		Version:    p.version,
		Difficulty: td,
		Head:       hash.Hex(),
	}
}

// Head retrieves a copy of the current head hash and total difficulty of the
// peer.
//返回当前链头块的hash及整体难度值。
func (p *peer) Head() (hash common.Hash, td *big.Int) {
	p.lock.RLock()	//加读锁。
	defer p.lock.RUnlock()	//解读锁。

	copy(hash[:], p.head[:])	//拷贝链头块hash值。
	return hash, new(big.Int).Set(p.td)	//返回hash值及块的整体难度。
}

// SetHead updates the head hash and total difficulty of the peer.
func (p *peer) SetHead(hash common.Hash, td *big.Int) {
	p.lock.Lock()
	defer p.lock.Unlock()

	copy(p.head[:], hash[:])
	p.td.Set(td)
}

// MarkBlock marks a block as known for the peer, ensuring that the block will
// never be propagated to this particular peer.
// 标记hash对应的块peer节点已经知道了.
func (p *peer) MarkBlock(hash common.Hash) {
	// If we reached the memory allowance, drop a previously known block hash
	for p.knownBlocks.Size() >= maxKnownBlocks {
		p.knownBlocks.Pop()
	}
	p.knownBlocks.Add(hash)
}

// MarkTransaction marks a transaction as known for the peer, ensuring that it
// will never be propagated to this particular peer.
//标记交易对当前的peer已知,这样可以防止此交易后续再被广播到这个节点.
func (p *peer) MarkTransaction(hash common.Hash) {
	// If we reached the memory allowance, drop a previously known transaction hash
	for p.knownTxs.Size() >= maxKnownTxs {
		p.knownTxs.Pop()
	}
	p.knownTxs.Add(hash)
}

func (p *peer) Send(msgcode uint64, data interface{}) error {
	return p2p.Send(p.rw, msgcode, data)
}

// SendTransactions sends transactions to the peer and includes the hashes
// in its transaction hash set for future reference.
func (p *peer) SendTransactions(txs types.Transactions) error {
	for _, tx := range txs {
		p.knownTxs.Add(tx.Hash())	//把给每个peer广播的消息添加到peer的已知交易列表中.
	}
	return p2p.Send(p.rw, TxMsg, txs)	//广播新的交易给peers.
}

// SendNewBlockHashes announces the availability of a number of blocks through
// a hash notification.
//发送新的块的hash.
// hashes: 哈希.
// numbers: 块编号.
func (p *peer) SendNewBlockHashes(hashes []common.Hash, numbers []uint64) error {
	for _, hash := range hashes {
		p.knownBlocks.Add(hash)		//把此hash添加到节点的已知块列表中.
	}
	request := make(newBlockHashesData, len(hashes))	//构造一个request.
	for i := 0; i < len(hashes); i++ {
		request[i].Hash = hashes[i]		//设置request的hash域.
		request[i].Number = numbers[i]	//设置request的number域,存储block number.
	}
	return p2p.Send(p.rw, NewBlockHashesMsg, request)	//发送新产生的块的hash.
}

// SendNewBlock propagates an entire block to a remote peer.
//向peer发送块.
func (p *peer) SendNewBlock(block *types.Block, td *big.Int) error {
	p.knownBlocks.Add(block.Hash())		//把当前的block hash添加到节点的knownBlocks列表中.
	return p2p.Send(p.rw, NewBlockMsg, []interface{}{block, td})	//向peer发送新的块.
}

// SendBlockHeaders sends a batch of block headers to the remote peer.
//发送一批headers给请求端.
func (p *peer) SendBlockHeaders(headers []*types.Header) error {
	return p2p.Send(p.rw, BlockHeadersMsg, headers)
}

// SendBlockBodies sends a batch of block contents to the remote peer.
func (p *peer) SendBlockBodies(bodies []*blockBody) error {
	return p2p.Send(p.rw, BlockBodiesMsg, blockBodiesData(bodies))
}

// SendBlockBodiesRLP sends a batch of block contents to the remote peer from
// an already RLP encoded format.
//获取并返回块的bodies.
func (p *peer) SendBlockBodiesRLP(bodies []rlp.RawValue) error {
	return p2p.Send(p.rw, BlockBodiesMsg, bodies)
}

// SendNodeDataRLP sends a batch of arbitrary internal data, corresponding to the
// hashes requested.
func (p *peer) SendNodeData(data [][]byte) error {
	return p2p.Send(p.rw, NodeDataMsg, data)
}

// SendReceiptsRLP sends a batch of transaction receipts, corresponding to the
// ones requested from an already RLP encoded format.
func (p *peer) SendReceiptsRLP(receipts []rlp.RawValue) error {
	return p2p.Send(p.rw, ReceiptsMsg, receipts)
}

// RequestOneHeader is a wrapper around the header query functions to fetch a
// single header. It is used solely by the fetcher.
//获取一个块的头.
func (p *peer) RequestOneHeader(hash common.Hash) error {
	p.Log().Debug("Fetching single header", "hash", hash)
	return p2p.Send(p.rw, GetBlockHeadersMsg, &getBlockHeadersData{Origin: hashOrNumber{Hash: hash}, Amount: uint64(1), Skip: uint64(0), Reverse: false})
}

// RequestHeadersByHash fetches a batch of blocks' headers corresponding to the
// specified header query, based on the hash of an origin block.
func (p *peer) RequestHeadersByHash(origin common.Hash, amount int, skip int, reverse bool) error {
	p.Log().Debug("Fetching batch of headers", "count", amount, "fromhash", origin, "skip", skip, "reverse", reverse)
	return p2p.Send(p.rw, GetBlockHeadersMsg, &getBlockHeadersData{Origin: hashOrNumber{Hash: origin}, Amount: uint64(amount), Skip: uint64(skip), Reverse: reverse})
}

// RequestHeadersByNumber fetches a batch of blocks' headers corresponding to the
// specified header query, based on the number of an origin block.
//通过块编号获得headers.
//origin: 最早尚未同步的块编号.
//amount: 每次请求最多获取的header数量.
//skip:	跳过的数量.
//reverse: 是否按照反序获取.
func (p *peer) RequestHeadersByNumber(origin uint64, amount int, skip int, reverse bool) error {
	p.Log().Debug("Fetching batch of headers", "count", amount, "fromnum", origin, "skip", skip, "reverse", reverse)
	return p2p.Send(p.rw, GetBlockHeadersMsg, &getBlockHeadersData{Origin: hashOrNumber{Number: origin}, Amount: uint64(amount), Skip: uint64(skip), Reverse: reverse})
}

// RequestBodies fetches a batch of blocks' bodies corresponding to the hashes
// specified.
func (p *peer) RequestBodies(hashes []common.Hash) error {
	p.Log().Debug("Fetching batch of block bodies", "count", len(hashes))
	return p2p.Send(p.rw, GetBlockBodiesMsg, hashes)
}

// RequestNodeData fetches a batch of arbitrary data from a node's known state
// data, corresponding to the specified hashes.
func (p *peer) RequestNodeData(hashes []common.Hash) error {
	p.Log().Debug("Fetching batch of state data", "count", len(hashes))
	return p2p.Send(p.rw, GetNodeDataMsg, hashes)
}

// RequestReceipts fetches a batch of transaction receipts from a remote node.
func (p *peer) RequestReceipts(hashes []common.Hash) error {
	p.Log().Debug("Fetching batch of receipts", "count", len(hashes))
	return p2p.Send(p.rw, GetReceiptsMsg, hashes)
}

// Handshake executes the eth protocol handshake, negotiating version number,
// network IDs, difficulties, head and genesis blocks.
func (p *peer) Handshake(network uint64, td *big.Int, head common.Hash, genesis common.Hash) error {
	// Send out own handshake in a new thread
	errc := make(chan error, 2)
	var status statusData // safe to read after two values have been received from errc

	go func() {
		errc <- p2p.Send(p.rw, StatusMsg, &statusData{
			ProtocolVersion: uint32(p.version),
			NetworkId:       network,
			TD:              td,
			CurrentBlock:    head,
			GenesisBlock:    genesis,
		})
	}()
	go func() {
		errc <- p.readStatus(network, &status, genesis)
	}()
	timeout := time.NewTimer(handshakeTimeout)
	defer timeout.Stop()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errc:
			if err != nil {
				return err
			}
		case <-timeout.C:
			return p2p.DiscReadTimeout
		}
	}
	p.td, p.head = status.TD, status.CurrentBlock
	return nil
}

func (p *peer) readStatus(network uint64, status *statusData, genesis common.Hash) (err error) {
	msg, err := p.rw.ReadMsg()
	if err != nil {
		return err
	}
	if msg.Code != StatusMsg {
		return errResp(ErrNoStatusMsg, "first msg has code %x (!= %x)", msg.Code, StatusMsg)
	}
	if msg.Size > ProtocolMaxMsgSize {
		return errResp(ErrMsgTooLarge, "%v > %v", msg.Size, ProtocolMaxMsgSize)
	}
	// Decode the handshake and make sure everything matches
	if err := msg.Decode(&status); err != nil {
		return errResp(ErrDecode, "msg %v: %v", msg, err)
	}
	if status.GenesisBlock != genesis {
		return errResp(ErrGenesisBlockMismatch, "%x (!= %x)", status.GenesisBlock[:8], genesis[:8])
	}
	if status.NetworkId != network {
		return errResp(ErrNetworkIdMismatch, "%d (!= %d)", status.NetworkId, network)
	}
	if int(status.ProtocolVersion) != p.version {
		return errResp(ErrProtocolVersionMismatch, "%d (!= %d)", status.ProtocolVersion, p.version)
	}
	return nil
}

// String implements fmt.Stringer.
func (p *peer) String() string {
	return fmt.Sprintf("Peer %s [%s]", p.id,
		fmt.Sprintf("eth/%2d", p.version),
	)
}

// peerSet represents the collection of active peers currently participating in
// the Ethereum sub-protocol.
type peerSet struct {
	peers  map[string]*peer
	lock   sync.RWMutex
	closed bool
}

// newPeerSet creates a new peer set to track the active participants.
func newPeerSet() *peerSet {
	return &peerSet{
		peers: make(map[string]*peer),
	}
}

// Register injects a new peer into the working set, or returns an error if the
// peer is already known.
func (ps *peerSet) Register(p *peer) error {
	ps.lock.Lock()
	defer ps.lock.Unlock()

	if ps.closed {
		return errClosed
	}
	if _, ok := ps.peers[p.id]; ok {
		return errAlreadyRegistered
	}
	ps.peers[p.id] = p
	return nil
}

// Unregister removes a remote peer from the active set, disabling any further
// actions to/from that particular entity.
func (ps *peerSet) Unregister(id string) error {
	ps.lock.Lock()
	defer ps.lock.Unlock()

	if _, ok := ps.peers[id]; !ok {
		return errNotRegistered
	}
	delete(ps.peers, id)
	return nil
}

// Peers returns all registered peers
func (ps *peerSet) Peers() map[string]*peer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	set := make(map[string]*peer)
	for id, p := range ps.peers {
		set[id] = p
	}
	return set
}

// Peer retrieves the registered peer with the given id.
func (ps *peerSet) Peer(id string) *peer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	return ps.peers[id]
}

// Len returns if the current number of peers in the set.
func (ps *peerSet) Len() int {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	return len(ps.peers)
}

// PeersWithoutBlock retrieves a list of peers that do not have a given block in
// their set of known hashes.
func (ps *peerSet) PeersWithoutBlock(hash common.Hash) []*peer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	list := make([]*peer, 0, len(ps.peers))
	for _, p := range ps.peers {
		if !p.knownBlocks.Has(hash) {
			list = append(list, p)
		}
	}
	return list
}

// PeersWithoutTx retrieves a list of peers that do not have a given transaction
// in their set of known hashes.
func (ps *peerSet) PeersWithoutTx(hash common.Hash) []*peer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	list := make([]*peer, 0, len(ps.peers))
	for _, p := range ps.peers {
		if !p.knownTxs.Has(hash) {
			list = append(list, p)
		}
	}
	return list
}

// BestPeer retrieves the known peer with the currently highest total difficulty.
//返回当前具有最大整体难度值的peer
// (这个peer说明具有最新的块，所以需要从这个链上进行同步。)
func (ps *peerSet) BestPeer() *peer {
	ps.lock.RLock()	//peerSet加读锁。
	defer ps.lock.RUnlock()		//函数退出解除读锁。

	var (
		bestPeer *peer	//最好的peer对象指针。
		bestTd   *big.Int	//最好peer的整体难度。
	)
	//遍历peers数组，获得具有最大td的peer并返回此peer。
	for _, p := range ps.peers {	//遍历此节点的peers列表。
		if _, td := p.Head(); bestPeer == nil || td.Cmp(bestTd) > 0 {
			bestPeer, bestTd = p, td
		}
	}
	return bestPeer		//返回具有最大td的peer。（td最大说明这个peer具有最长的链。）
}

// Close disconnects all peers.
// No new peers can be registered after Close has returned.
func (ps *peerSet) Close() {
	ps.lock.Lock()
	defer ps.lock.Unlock()

	for _, p := range ps.peers {
		p.Disconnect(p2p.DiscQuitting)
	}
	ps.closed = true
}
