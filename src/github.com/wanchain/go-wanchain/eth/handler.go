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
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wanchain/go-wanchain/common"
	"github.com/wanchain/go-wanchain/consensus"
	"github.com/wanchain/go-wanchain/core"
	"github.com/wanchain/go-wanchain/core/types"
	"github.com/wanchain/go-wanchain/eth/downloader"
	"github.com/wanchain/go-wanchain/eth/fetcher"
	"github.com/wanchain/go-wanchain/ethdb"
	"github.com/wanchain/go-wanchain/event"
	"github.com/wanchain/go-wanchain/log"
	"github.com/wanchain/go-wanchain/p2p"
	"github.com/wanchain/go-wanchain/p2p/discover"
	"github.com/wanchain/go-wanchain/params"
	"github.com/wanchain/go-wanchain/rlp"
	"github.com/wanchain/go-wanchain/crypto"
)

const (
	softResponseLimit = 2 * 1024 * 1024 // Target maximum size of returned blocks, headers or node data.
	estHeaderRlpSize  = 500             // Approximate size of an RLP encoded block header

	// txChanSize is the size of channel listening to TxPreEvent.
	// The number is referenced from the size of tx pool.
	txChanSize = 4096	//交易
)

var (
	daoChallengeTimeout = 15 * time.Second // Time allowance for a node to reply to the DAO handshake challenge
)

// errIncompatibleConfig is returned if the requested protocols and configs are
// not compatible (low protocol version restrictions and high requirements).
var errIncompatibleConfig = errors.New("incompatible configuration")

func errResp(code errCode, format string, v ...interface{}) error {
	return fmt.Errorf("%v - %v", code, fmt.Sprintf(format, v...))
}

//ProtocolManager是管理node之间p2p通信的顶层结构。
type ProtocolManager struct {
	networkId uint64	//网络id.

	fastSync  uint32 // Flag whether fast sync is enabled (gets disabled if we already have blocks)	//是否使用fastSync方式进行同步.
	acceptTxs uint32 // Flag whether we're considered synchronised (enables transaction processing) //是否可以接收及处理交易的标志.是否已经完成了同步的标志.

	txpool      txPool
	blockchain  *core.BlockChain
	chaindb     ethdb.Database
	chainconfig *params.ChainConfig
	maxPeers    int		//p2p的最大通信对端数。

	downloader *downloader.Downloader  //(被动获取)负责所有向相邻个体主动发起的同步流程。
	fetcher    *fetcher.Fetcher  //(主动获取)负责累积所有其他个体(有可能是peer，也有可能节点内的其他模块)发送来的有关新数据的宣布消息，并在自身对照后，安排相应的获取请求。
	peers      *peerSet		//peers列表。
	                        //在以太坊网络中，所有进行通信的两个peer都必须率先经过相互注册，并被添加到各自的缓存peer列表中，也就是peerset{}对象中。
	                        //这样的两个peer，可以称之为相邻。所以，远端个体，如果处于可通信状态，那么必定是相邻的。

	SubProtocols []p2p.Protocol		//当前Node支持的所有协议.

	eventMux      *event.TypeMux
	txCh          chan core.TxPreEvent	//交易通知接收通道，异步，buffer为4096
	txSub         event.Subscription	//订阅交易通知，
	minedBlockSub *event.TypeMuxSubscription	//存储

	// channels for fetcher, syncer, txsyncLoop
	newPeerCh   chan *peer
	txsyncCh    chan *txsync
	quitSync    chan struct{}
	noMorePeers chan struct{}

	// wait group is used for graceful shutdowns during downloading
	// and processing
	wg sync.WaitGroup
	engine   consensus.Engine
}

// NewProtocolManager returns a new ethereum sub protocol manager. The Ethereum sub protocol manages peers capable
// with the ethereum network.
// 创建node的protocolManager.
func NewProtocolManager(config *params.ChainConfig, mode downloader.SyncMode, networkId uint64, mux *event.TypeMux, txpool txPool, engine consensus.Engine, blockchain *core.BlockChain, chaindb ethdb.Database) (*ProtocolManager, error) {
	// Create the protocol manager with the base fields
	manager := &ProtocolManager{
		networkId:   networkId,
		eventMux:    mux,
		txpool:      txpool,
		blockchain:  blockchain,
		chaindb:     chaindb,
		chainconfig: config,
		peers:       newPeerSet(),
		newPeerCh:   make(chan *peer),
		noMorePeers: make(chan struct{}),
		txsyncCh:    make(chan *txsync),
		quitSync:    make(chan struct{}),
		engine:      engine,
	}

	//共识都实现了consensus.Handler接口,此处把共识对象强制转换为consensus.Handler接口,然后设置共识机制的broadcaster为当前eth的protocol manager.
	if handler, ok := manager.engine.(consensus.Handler); ok {		//如果能够转换成对应的接口,那么ok返回true,如果对象类型不能断言成指定的类型,那么ok返回false.
		handler.SetBroadcaster(manager)		//设置共识对象的broadcaster为eth的protocol manager.
	}

	// Figure out whether to allow fast sync or not
	// 什么时候fastsync模式才生效?
	//     只有在用户配置了fastsync模式并且node当前的链上只有创世块的时候fastsync才生效,
	//     如果当前链上已经有块了,那么即使用户配置了fastsync模式,在代码中也会强制转换成fullsync模式.
	if mode == downloader.FastSync && blockchain.CurrentBlock().NumberU64() > 0 {
		log.Warn("Blockchain not empty, fast sync disabled")
		mode = downloader.FullSync
	}
	if mode == downloader.FastSync {
		manager.fastSync = uint32(1)
	}
	protocol := engine.Protocol()	//获得共识协议.
	// Initiate a sub-protocol for every implemented version we can handle
	manager.SubProtocols = make([]p2p.Protocol, 0, len(protocol.Versions))	//初始化SubProtocols字段.
	for i, version := range protocol.Versions {
		// Skip protocol version if incompatible with the mode of operation
		if mode == downloader.FastSync && version < eth63 {
			continue
		}
		// Compatible; initialise the sub-protocol
		version := version // Closure for the run
		//此处把共识算法子协议添加到了SubProtocols中.
		manager.SubProtocols = append(manager.SubProtocols, p2p.Protocol{	//构造p2p子协议并填充到SubProtocols中.
			Name:    protocol.Name,
			Version: version,
			Length:  protocol.Lengths[i],
			Run: func(p *p2p.Peer, rw p2p.MsgReadWriter) error {
				peer := manager.newPeer(int(version), p, rw)
				select {
				case manager.newPeerCh <- peer:
					manager.wg.Add(1)
					defer manager.wg.Done()
					return manager.handle(peer)
				case <-manager.quitSync:
					return p2p.DiscQuitting
				}
			},
			NodeInfo: func() interface{} {	//获得节点信息的函数.
				return manager.NodeInfo()
			},
			PeerInfo: func(id discover.NodeID) interface{} {	//获得peer信息的函数.
				if p := manager.peers.Peer(fmt.Sprintf("%x", id[:8])); p != nil {
					return p.Info()
				}
				return nil
			},
		})
	}
	if len(manager.SubProtocols) == 0 {
		return nil, errIncompatibleConfig
	}
	// Construct the different synchronisation mechanisms
	//构造downloader.
	manager.downloader = downloader.New(mode, chaindb, manager.eventMux, blockchain, nil, manager.removePeer)

	//块头验证函数.
	validator := func(header *types.Header) error {
		return engine.VerifyHeader(blockchain, header, true)	//调用共识算法中的verifyHeader函数.
	}

	//获得区块链高度的函数.
	heighter := func() uint64 {
		return blockchain.CurrentBlock().NumberU64()
	}

	//插入多个块到链上的函数.
	inserter := func(blocks types.Blocks) (int, error) {
		// If fast sync is running, deny importing weird blocks
		//如果当前的模式是fastsync模式,那么对于其他Node广播过来的块直接丢弃,不允许插入到链上.(在ibft第一次启动挖矿的时候日志中会打印这个问题.)
		if atomic.LoadUint32(&manager.fastSync) == 1 {	//处在fastSync模式的时候不允许插入块.
			log.Warn("Discarded bad propagated(蔓延) block", "number", blocks[0].Number(), "hash", blocks[0].Hash())
			return 0, nil
		}
		atomic.StoreUint32(&manager.acceptTxs, 1) // Mark initial sync done on any fetcher import 在fetcher导入块之前首先把同步标志设置为完成.
		return manager.blockchain.InsertChain(blocks)	//把块插入到本地区块链上.
	}

	//构造fetcher.
	manager.fetcher = fetcher.New(blockchain.GetBlockByHash, validator, manager.BroadcastBlock, heighter, inserter, manager.removePeer)

	return manager, nil
}

//peer删除函数.
func (pm *ProtocolManager) removePeer(id string) {
	// Short circuit if the peer was already removed
	peer := pm.peers.Peer(id) //获得peer对象.
	if peer == nil {
		return
	}
	log.Debug("Removing Wanchain peer", "peer", id)

	// Unregister the peer from the downloader and Ethereum peer set
	pm.downloader.UnregisterPeer(id)	//解除注册peer.
	if err := pm.peers.Unregister(id); err != nil {
		log.Error("Peer removal failed", "peer", id, "err", err)
	}
	// Hard disconnect at the networking layer
	if peer != nil {
		peer.Peer.Disconnect(p2p.DiscUselessPeer)		//断开peer与本节点的p2p连接.
	}
}

//p2p通信过程的全面启动。
func (pm *ProtocolManager) Start(maxPeers int) {
	pm.maxPeers = maxPeers  //当前节点的最大对端数。

	// broadcast transactions
	pm.txCh = make(chan core.TxPreEvent, txChanSize)	//构建交易通知通道,异步通信，buffer为4096
	pm.txSub = pm.txpool.SubscribeTxPreEvent(pm.txCh)	//订阅交易通知。
	go pm.txBroadcastLoop()	//广播交易对象，一旦接收到有关新交易的事件，会立即调用BroadcastTx()函数广播给那些尚无该交易对象的相邻个体。

	// broadcast mined blocks
	pm.minedBlockSub = pm.eventMux.Subscribe(core.NewMinedBlockEvent{})  //广播新挖掘出的区块。
	go pm.minedBroadcastLoop()  //持续等待本个体的新挖掘出区块事件，然后立即广播给需要的相邻个体。

	// start sync handlers
	go pm.syncer()  //定时与相邻个体进行区块全链的强制同步。
	                // syncer()首先启动fetcher成员，然后进入一个无限循环，每次循环中都会向相邻peer列表中“最优”的那个peer作一次区块全链同步。
	                //所谓最优就是peer中维护的区块链的total difficulty最高。
	go pm.txsyncLoop()  //将新出现的交易对象均匀的同步给相邻个体。
}

func (pm *ProtocolManager) Stop() {
	log.Info("Stopping Wanchain protocol")

	pm.txSub.Unsubscribe()         // quits txBroadcastLoop
	pm.minedBlockSub.Unsubscribe() // quits blockBroadcastLoop

	// Quit the sync loop.
	// After this send has completed, no new peers will be accepted.
	pm.noMorePeers <- struct{}{}

	// Quit fetcher, txsyncLoop.
	close(pm.quitSync)

	// Disconnect existing sessions.
	// This also closes the gate for any new registrations on the peer set.
	// sessions which are already established but not added to pm.peers yet
	// will exit when they try to register.
	pm.peers.Close()

	// Wait for all peer handler goroutines and the loops to come down.
	pm.wg.Wait()

	log.Info("Wanchain protocol stopped")
}

func (pm *ProtocolManager) newPeer(pv int, p *p2p.Peer, rw p2p.MsgReadWriter) *peer {
	return newPeer(pv, p, newMeteredMsgWriter(rw))
}

// handle is the callback invoked to manage the life cycle of an eth peer. When
// this function terminates, the peer is disconnected.
//供peer调用的函数。
func (pm *ProtocolManager) handle(p *peer) error {
	if pm.peers.Len() >= pm.maxPeers {
		return p2p.DiscTooManyPeers
	}
	p.Log().Debug("Wanchain peer connected", "name", p.Name())

	// Execute the Ethereum handshake
	td, head, genesis := pm.blockchain.Status()
	if err := p.Handshake(pm.networkId, td, head, genesis); err != nil {
		p.Log().Debug("Wanchain handshake failed", "err", err)
		return err
	}
	if rw, ok := p.rw.(*meteredMsgReadWriter); ok {
		rw.Init(p.version)
	}
	// Register the peer locally
	if err := pm.peers.Register(p); err != nil {
		p.Log().Error("Wanchain peer registration failed", "err", err)
		return err
	}
	defer pm.removePeer(p.id)

	// Register the peer in the downloader. If the downloader considers it banned, we disconnect
	if err := pm.downloader.RegisterPeer(p.id, p.version, p); err != nil {
		return err
	}
	// Propagate existing transactions. new transactions appearing
	// after this will be sent via broadcasts.
	pm.syncTransactions(p)

	// If we're DAO hard-fork aware, validate any remote peer with regard to the hard-fork
	//if daoBlock := pm.chainconfig.DAOForkBlock; daoBlock != nil {
	//	// Request the peer's DAO fork header for extra-data validation
	//	if err := p.RequestHeadersByNumber(daoBlock.Uint64(), 1, 0, false); err != nil {
	//		return err
	//	}
	//	// Start a timer to disconnect if the peer doesn't reply in time
	//	p.forkDrop = time.AfterFunc(daoChallengeTimeout, func() {
	//		p.Log().Debug("Timed out DAO fork-check, dropping")
	//		pm.removePeer(p.id)
	//	})
	//	// Make sure it's cleaned up if the peer dies off
	//	defer func() {
	//		if p.forkDrop != nil {
	//			p.forkDrop.Stop()
	//			p.forkDrop = nil
	//		}
	//	}()
	//}
	// main loop. handle incoming messages.
	for {
		if err := pm.handleMsg(p); err != nil {
			p.Log().Debug("Wanchain message handling failed", "err", err)
			return err
		}
	}
}

// handleMsg is invoked whenever an inbound message is received from a remote
// peer. The remote connection is torn down upon returning any error.
//handleMsg用来处理与peer之间的消息收发.
//当peer发来消息的时候,handleMsg()函数就用来处理接收到的消息.
//当node广播交易之后,与其相连的peer就是通过这个函数来获得交易并进行后续处理的.
func (pm *ProtocolManager) handleMsg(p *peer) error {
	// Read the next message from the remote peer, and ensure it's fully consumed
	msg, err := p.rw.ReadMsg() //读取消息.
	if err != nil {
		return err
	}
	if msg.Size > ProtocolMaxMsgSize {
		return errResp(ErrMsgTooLarge, "%v > %v", msg.Size, ProtocolMaxMsgSize)
	}
	defer msg.Discard() //消息在函数结束就被销毁了.

	if handler, ok := pm.engine.(consensus.Handler); ok {
		pubKey, err := p.ID().Pubkey()
		if err != nil {
			return err
		}
		addr := crypto.PubkeyToAddress(*pubKey)
		handled, err := handler.HandleMsg(addr, msg)
		if handled {
			return err
		}
	}

	// Handle the message depending on its contents
	//根据消息类型来处理消息.
	switch {
	case msg.Code == StatusMsg:
		// Status messages should never arrive after the handshake
		return errResp(ErrExtraStatusMsg, "uncontrolled status message")

	// Block header query, collect the requested headers and reply
	case msg.Code == GetBlockHeadersMsg:		//响应peer发送过来的GetBlockHeadersMsg,获取结果并返回.
		// Decode the complex header query
		var query getBlockHeadersData
		if err := msg.Decode(&query); err != nil {	//从msg中解析出指定结构体.
			return errResp(ErrDecode, "%v: %v", msg, err)
		}
		hashMode := query.Origin.Hash != (common.Hash{})	//获得开始块的hash,如果获取到那么使用hash读取header,如果没有获取到hash,那么使用number获取header.

		// Gather headers until the fetch or network limits is reached
		var (
			bytes   common.StorageSize
			headers []*types.Header
			unknown bool
		)
		for !unknown && len(headers) < int(query.Amount) && bytes < softResponseLimit && len(headers) < downloader.MaxHeaderFetch {
			// Retrieve the next header satisfying the query
			//获得开始块的hash.
			var origin *types.Header
			if hashMode {
				origin = pm.blockchain.GetHeaderByHash(query.Origin.Hash)	//根据hash读取块.
			} else {
				origin = pm.blockchain.GetHeaderByNumber(query.Origin.Number)	//根据number读取块.
			}
			if origin == nil {
				break
			}
			number := origin.Number.Uint64()	//获得初始块号.
			headers = append(headers, origin)	//把第一个块添加到返回数组.
			bytes += estHeaderRlpSize		//预估增加的字节数.

			// Advance to the next header of the query
			switch {
			case query.Origin.Hash != (common.Hash{}) && query.Reverse:	//基于hash的反向查找.
				// Hash based traversal towards the genesis block
				for i := 0; i < int(query.Skip)+1; i++ {
					if header := pm.blockchain.GetHeader(query.Origin.Hash, number); header != nil {
						query.Origin.Hash = header.ParentHash
						number--
					} else {
						unknown = true
						break
					}
				}
			case query.Origin.Hash != (common.Hash{}) && !query.Reverse:	//基于hash的正向查找.
				// Hash based traversal towards the leaf block
				var (
					current = origin.Number.Uint64()
					next    = current + query.Skip + 1
				)
				if next <= current {
					infos, _ := json.MarshalIndent(p.Peer.Info(), "", "  ")
					p.Log().Warn("GetBlockHeaders skip overflow attack", "current", current, "skip", query.Skip, "next", next, "attacker", infos)
					unknown = true
				} else {
					if header := pm.blockchain.GetHeaderByNumber(next); header != nil {
						if pm.blockchain.GetBlockHashesFromHash(header.Hash(), query.Skip+1)[query.Skip] == query.Origin.Hash {
							query.Origin.Hash = header.Hash()
						} else {
							unknown = true
						}
					} else {
						unknown = true
					}
				}
			case query.Reverse:		//基于块号的反向查找.
				// Number based traversal towards the genesis block
				if query.Origin.Number >= query.Skip+1 {
					query.Origin.Number -= (query.Skip + 1)
				} else {
					unknown = true
				}

			case !query.Reverse:	//基于块号的正向查找.
				// Number based traversal towards the leaf block
				query.Origin.Number += (query.Skip + 1)		//每间隔skip个块取一个header.
			}
		}
		return p.SendBlockHeaders(headers)		//发送给请求端获得到的headers列表信息.

	case msg.Code == BlockHeadersMsg:	//返回批量的headers查询结果,此结果是对之前的批量headers查询请求的响应.
		// A batch of headers arrived to one of our previous requests
		var headers []*types.Header
		if err := msg.Decode(&headers); err != nil {	//解码消息,把返回的headers存储到局部变量headers数组中.
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}

		// If no headers were received, but we're expending a DAO fork check, maybe it's that
		//if len(headers) == 0 && p.forkDrop != nil {
		//	// Possibly an empty reply to the fork header checks, sanity check TDs
		//	verifyDAO := true
		//
		//	// If we already have a DAO header, we can check the peer's TD against it. If
		//	// the peer's ahead of this, it too must have a reply to the DAO check
		//	//if daoHeader := pm.blockchain.GetHeaderByNumber(pm.chainconfig.DAOForkBlock.Uint64()); daoHeader != nil {
		//	//	if _, td := p.Head(); td.Cmp(pm.blockchain.GetTd(daoHeader.Hash(), daoHeader.Number.Uint64())) >= 0 {
		//	//		verifyDAO = false
		//	//	}
		//	//}
		//	// If we're seemingly on the same chain, disable the drop timer
		//	if verifyDAO {
		//		p.Log().Debug("Seems to be on the same side of the DAO fork")
		//		p.forkDrop.Stop()
		//		p.forkDrop = nil
		//		return nil
		//	}
		//}
		// Filter out any explicitly requested headers, deliver the rest to the downloader
		filter := len(headers) == 1		//如果是新块广播而获取块头的话,那么此处headers就是1,这个时候代码会走到下面的 if filter {} 流程中.
		if filter {
			// If it's a potential DAO fork check, validate against the rules
			//if p.forkDrop != nil && pm.chainconfig.DAOForkBlock.Cmp(headers[0].Number) == 0 {
			//	// Disable the fork drop timer
			//	p.forkDrop.Stop()
			//	p.forkDrop = nil
			//
			//	// Validate the header and either drop the peer or continue
			//	if err := misc.VerifyDAOHeaderExtraData(pm.chainconfig, headers[0]); err != nil {
			//		p.Log().Debug("Verified to be on the other side of the DAO fork, dropping")
			//		return err
			//	}
			//	p.Log().Debug("Verified to be on the same side of the DAO fork")
			//	return nil
			//}
			// Irrelevant of the fork checks, send the header to the fetcher just in case
			headers = pm.fetcher.FilterHeaders(p.id, headers, time.Now())
		}

		if len(headers) > 0 || !filter {
			err := pm.downloader.DeliverHeaders(p.id, headers)	//把收到的headers放入downloader的headerCh中.
			if err != nil {
				log.Debug("Failed to deliver headers", "err", err)
			}
		}

	case msg.Code == GetBlockBodiesMsg:	//收到并处理批量获取块body的请求并返回结果.
		// Decode the retrieval message
		msgStream := rlp.NewStream(msg.Payload, uint64(msg.Size))
		if _, err := msgStream.List(); err != nil {
			return err
		}
		// Gather blocks until the fetch or network limits is reached
		var (
			hash   common.Hash
			bytes  int
			bodies []rlp.RawValue
		)
		for bytes < softResponseLimit && len(bodies) < downloader.MaxBlockFetch {
			// Retrieve the hash of the next block
			if err := msgStream.Decode(&hash); err == rlp.EOL {
				break
			} else if err != nil {
				return errResp(ErrDecode, "msg %v: %v", msg, err)
			}
			// Retrieve the requested block body, stopping if enough was found
			if data := pm.blockchain.GetBodyRLP(hash); len(data) != 0 {
				bodies = append(bodies, data)
				bytes += len(data)
			}
		}
		return p.SendBlockBodiesRLP(bodies)	//通过p2p网络发送响应消息给请求端.

	case msg.Code == BlockBodiesMsg:	//处理peer节点发送过来的bodies.
		// A batch of block bodies arrived to one of our previous requests
		var request blockBodiesData
		if err := msg.Decode(&request); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		// Deliver them all to the downloader for queuing
		trasactions := make([][]*types.Transaction, len(request))
		uncles := make([][]*types.Header, len(request))

		//从收到的bodies里面获取交易及uncles. 此for循环把所有body中的交易都放在了一起.
		for i, body := range request {
			trasactions[i] = body.Transactions
			uncles[i] = body.Uncles
		}
		// Filter out any explicitly requested bodies, deliver the rest to the downloader
		filter := len(trasactions) > 0 || len(uncles) > 0
		if filter {
			trasactions, uncles = pm.fetcher.FilterBodies(p.id, trasactions, uncles, time.Now())	//在采用ibft共识时候,uncles总是为0,因此FilterBodies里的流程不会过滤任何内容.
		}
		if len(trasactions) > 0 || len(uncles) > 0 || !filter {
			err := pm.downloader.DeliverBodies(p.id, trasactions, uncles)	//传递过来的trasactions包括一批bodies中的交易.
			if err != nil {
				log.Debug("Failed to deliver bodies", "err", err)
			}
		}

	case p.version >= eth63 && msg.Code == GetNodeDataMsg:
		// Decode the retrieval message
		msgStream := rlp.NewStream(msg.Payload, uint64(msg.Size))
		if _, err := msgStream.List(); err != nil {
			return err
		}
		// Gather state data until the fetch or network limits is reached
		var (
			hash  common.Hash
			bytes int
			data  [][]byte
		)
		for bytes < softResponseLimit && len(data) < downloader.MaxStateFetch {
			// Retrieve the hash of the next state entry
			if err := msgStream.Decode(&hash); err == rlp.EOL {
				break
			} else if err != nil {
				return errResp(ErrDecode, "msg %v: %v", msg, err)
			}
			// Retrieve the requested state entry, stopping if enough was found
			if entry, err := pm.chaindb.Get(hash.Bytes()); err == nil {
				data = append(data, entry)
				bytes += len(entry)
			}
		}
		return p.SendNodeData(data)

	case p.version >= eth63 && msg.Code == NodeDataMsg:
		// A batch of node state data arrived to one of our previous requests
		var data [][]byte
		if err := msg.Decode(&data); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		// Deliver all to the downloader
		if err := pm.downloader.DeliverNodeData(p.id, data); err != nil {
			log.Debug("Failed to deliver node state data", "err", err)
		}

	case p.version >= eth63 && msg.Code == GetReceiptsMsg:
		// Decode the retrieval message
		msgStream := rlp.NewStream(msg.Payload, uint64(msg.Size))
		if _, err := msgStream.List(); err != nil {
			return err
		}
		// Gather state data until the fetch or network limits is reached
		var (
			hash     common.Hash
			bytes    int
			receipts []rlp.RawValue
		)
		for bytes < softResponseLimit && len(receipts) < downloader.MaxReceiptFetch {
			// Retrieve the hash of the next block
			if err := msgStream.Decode(&hash); err == rlp.EOL {
				break
			} else if err != nil {
				return errResp(ErrDecode, "msg %v: %v", msg, err)
			}
			// Retrieve the requested block's receipts, skipping if unknown to us
			results := core.GetBlockReceipts(pm.chaindb, hash, core.GetBlockNumber(pm.chaindb, hash))
			if results == nil {
				if header := pm.blockchain.GetHeaderByHash(hash); header == nil || header.ReceiptHash != types.EmptyRootHash {
					continue
				}
			}
			// If known, encode and queue for response packet
			if encoded, err := rlp.EncodeToBytes(results); err != nil {
				log.Error("Failed to encode receipt", "err", err)
			} else {
				receipts = append(receipts, encoded)
				bytes += len(encoded)
			}
		}
		return p.SendReceiptsRLP(receipts)

	case p.version >= eth63 && msg.Code == ReceiptsMsg:
		// A batch of receipts arrived to one of our previous requests
		var receipts [][]*types.Receipt
		if err := msg.Decode(&receipts); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		// Deliver all to the downloader
		if err := pm.downloader.DeliverReceipts(p.id, receipts); err != nil {
			log.Debug("Failed to deliver receipts", "err", err)
		}

	case msg.Code == NewBlockHashesMsg:
		var announces newBlockHashesData
		if err := msg.Decode(&announces); err != nil {
			return errResp(ErrDecode, "%v: %v", msg, err)
		}
		// Mark the hashes as present at the remote node	//标记这个hash对p节点是已知的(p节点即为发送这个块hash的节点.)
		for _, block := range announces {
			p.MarkBlock(block.Hash)		//标记peer节点(也就是发送这个块hash的节点)已经知道了这个块了,这样本节点再次广播块的时候就不会再次发送给这个peer节点了.
		}

		//如果这个块hash在本地链上没有,那么加入到unknown列表用于后续获取块实体.
		// Schedule all the unknown hashes for retrieval
		unknown := make(newBlockHashesData, 0, len(announces))
		for _, block := range announces {
			if !pm.blockchain.HasBlock(block.Hash, block.Number) {
				unknown = append(unknown, block)
			}
		}
		for _, block := range unknown {	//对于所有未知块列表中的hash,通知fetcher获取这个块.
			pm.fetcher.Notify(p.id, block.Hash, block.Number, time.Now(), p.RequestOneHeader, p.RequestBodies)
		}

	case msg.Code == NewBlockMsg:
		// Retrieve and decode the propagated block
		var request newBlockData
		if err := msg.Decode(&request); err != nil {
			return errResp(ErrDecode, "%v: %v", msg, err)
		}
		request.Block.ReceivedAt = msg.ReceivedAt
		request.Block.ReceivedFrom = p

		// Mark the peer as owning the block and schedule it for import
		p.MarkBlock(request.Block.Hash())
		pm.fetcher.Enqueue(p.id, request.Block)

		// Assuming the block is importable by the peer, but possibly not yet done so,
		// calculate the head hash and TD that the peer truly must have.
		var (
			trueHead = request.Block.ParentHash()
			trueTD   = new(big.Int).Sub(request.TD, request.Block.Difficulty())
		)
		// Update the peers total difficulty if better than the previous
		if _, td := p.Head(); trueTD.Cmp(td) > 0 {
			p.SetHead(trueHead, trueTD)

			// Schedule a sync if above ours. Note, this will not fire a sync for a gap of
			// a singe block (as the true TD is below the propagated block), however this
			// scenario should easily be covered by the fetcher.
			currentBlock := pm.blockchain.CurrentBlock()
			if trueTD.Cmp(pm.blockchain.GetTd(currentBlock.Hash(), currentBlock.NumberU64())) > 0 {
				go pm.synchronise(p)
			}
		}

	case msg.Code == TxMsg:	//处理从peer发送过来的交易.
		log.Info("guoxu get a new transaction from peer.")
		// Transactions arrived, make sure we have a valid and fresh chain to handle them
		//判断当前是否可以接受交易,如果可以那么就继续处理,如果不可以则直接跳出case.
		if atomic.LoadUint32(&pm.acceptTxs) == 0 {
			break
		}
		// Transactions can be processed, parse all of them and deliver to the pool
		var txs []*types.Transaction
		if err := msg.Decode(&txs); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		//如果当前能够处理交易的话,那么处理每一个传递过来的交易.
		for i, tx := range txs {
			// Validate and mark the remote transaction
			if tx == nil {
				return errResp(ErrDecode, "transaction %d is nil", i)
			}
			p.MarkTransaction(tx.Hash()) //把交易消息标记为p对应的peer已经知道了这交易这样就会防止此交易再次被广播给peer p.
		}
		//把交易放入到本地的txpool中.
		pm.txpool.AddRemotes(txs)

	default:
		return errResp(ErrInvalidMsgCode, "%v", msg.Code)
	}
	return nil
}

//实现了broadcaster的Enqueue()方法.
func (pm *ProtocolManager) Enqueue(id string, block *types.Block) {
	pm.fetcher.Enqueue(id, block)
}

// BroadcastBlock will either propagate a block to a subset of it's peers, or
// will only announce it's availability (depending what's requested).
//向peer广播块.	propagate:蔓延,扩散.
// block: 待广播区块.
// propagate: 是否广播块还是只广播块的hash. true: 广播块; false: 广播块的hash.
//注意: 此函数会向部分peer广播块本身,向全部块广播块hash.
func (pm *ProtocolManager) BroadcastBlock(block *types.Block, propagate bool) {
	hash := block.Hash()	//获得块hash.
	peers := pm.peers.PeersWithoutBlock(hash)	//获得当前节点的peer中尚未获得这个块的peer.

	// If propagation is requested, send to a subset of the peer
	if propagate {
		// Calculate the TD of the block (it's not imported yet, so block.Td is not valid)
		var td *big.Int
		if parent := pm.blockchain.GetBlock(block.ParentHash(), block.NumberU64()-1); parent != nil {
			td = new(big.Int).Add(block.Difficulty(), pm.blockchain.GetTd(block.ParentHash(), block.NumberU64()-1))	//td = 当前链td + 当前块的td.
		} else {
			log.Error("Propagating dangling block", "number", block.Number(), "hash", hash)
			return
		}
		// Send the block to a subset of our peers	//获得peer列表,
		transfer := peers[:int(math.Sqrt(float64(len(peers))))]	//获得部分peer列表,不是全部的peer都发送. math.Sqrt()开平方.
		for _, peer := range transfer {
			peer.SendNewBlock(block, td)	//向peer发送block.
		}
		log.Trace("Propagated block", "hash", hash, "recipients", len(transfer), "duration", common.PrettyDuration(time.Since(block.ReceivedAt)))
		return
	}
	// Otherwise if the block is indeed in out own chain, announce it
	if pm.blockchain.HasBlock(hash, block.NumberU64()) {
		for _, peer := range peers {
			peer.SendNewBlockHashes([]common.Hash{hash}, []uint64{block.NumberU64()})	//发送新块的hash.
		}
		log.Trace("Announced block", "hash", hash, "recipients", len(peers), "duration", common.PrettyDuration(time.Since(block.ReceivedAt)))
	}
}

// BroadcastTx will propagate a transaction to all peers which are not known to
// already have the given transaction.
func (pm *ProtocolManager) BroadcastTx(hash common.Hash, tx *types.Transaction) {
	// Broadcast transaction to a batch of peers not knowing about it
	peers := pm.peers.PeersWithoutTx(hash)
	//FIXME include this again: peers = peers[:int(math.Sqrt(float64(len(peers))))]
	for _, peer := range peers {
		peer.SendTransactions(types.Transactions{tx})
	}
	log.Trace("Broadcast transaction", "hash", hash, "recipients", len(peers))
}

// Mined broadcast loop
//挖掘出新块的事件响应循环.
func (self *ProtocolManager) minedBroadcastLoop() {
	// automatically stops if unsubscribe
	for obj := range self.minedBlockSub.Chan() {	//对所有收到的新挖掘区块做顺序处理.
		switch ev := obj.Data.(type) {		//获得事件.
		case core.NewMinedBlockEvent:	//响应NewMinedBlockEvent,处理新区块.
			self.BroadcastBlock(ev.Block, true)  // First propagate block to peers
			self.BroadcastBlock(ev.Block, false) // Only then announce to the rest
		}
	}
}

func (self *ProtocolManager) txBroadcastLoop() {
	for {
		select {
		case event := <-self.txCh:
			self.BroadcastTx(event.Tx.Hash(), event.Tx)

		// Err() channel will be closed when unsubscribing.
		case <-self.txSub.Err():
			return
		}
	}
}

// EthNodeInfo represents a short summary of the Ethereum sub-protocol metadata known
// about the host peer.
type EthNodeInfo struct {
	Network    uint64      `json:"network"`    // Ethereum network ID (1=Frontier, 2=Morden, Ropsten=3)
	Difficulty *big.Int    `json:"difficulty"` // Total difficulty of the host's blockchain
	Genesis    common.Hash `json:"genesis"`    // SHA3 hash of the host's genesis block
	Head       common.Hash `json:"head"`       // SHA3 hash of the host's best owned block
}

// NodeInfo retrieves some protocol metadata about the running host node.
func (self *ProtocolManager) NodeInfo() *EthNodeInfo {
	currentBlock := self.blockchain.CurrentBlock()
	return &EthNodeInfo{
		Network:    self.networkId,
		Difficulty: self.blockchain.GetTd(currentBlock.Hash(), currentBlock.NumberU64()),
		Genesis:    self.blockchain.Genesis().Hash(),
		Head:       currentBlock.Hash(),
	}
}

func (self *ProtocolManager) FindPeers(targets map[common.Address]bool) map[common.Address]consensus.Peer {
	m := make(map[common.Address]consensus.Peer)
	for _, p := range self.peers.Peers() {
		pubKey, err := p.ID().Pubkey()
		if err != nil {
			continue
		}
		addr := crypto.PubkeyToAddress(*pubKey)
		if targets[addr] {
			m[addr] = p
		}
	}
	return m
}
