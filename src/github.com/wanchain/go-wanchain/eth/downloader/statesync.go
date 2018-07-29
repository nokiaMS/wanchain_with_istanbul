// Copyright 2017 The go-ethereum Authors
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

package downloader

import (
	"fmt"
	"hash"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wanchain/go-wanchain/common"
	"github.com/wanchain/go-wanchain/core/state"
	"github.com/wanchain/go-wanchain/crypto/sha3"
	"github.com/wanchain/go-wanchain/ethdb"
	"github.com/wanchain/go-wanchain/log"
	"github.com/wanchain/go-wanchain/trie"
)

// stateReq represents a batch of state fetch requests groupped together into
// a single data retrieval network packet.
//stateReq是打包在一起的一批状态获取请求.
type stateReq struct {
	items    []common.Hash              // Hashes of the state items to download
	tasks    map[common.Hash]*stateTask // Download tasks to track previous attempts
	timeout  time.Duration              // Maximum round trip time for this to complete	//请求完成的最长时间.
	timer    *time.Timer                // Timer to fire when the RTT timeout expires
	peer     *peerConnection            // Peer that we're requesting from		//执行请求的peer对象.即从哪个peer请求数据.
	response [][]byte                   // Response data of the peer (nil for timeouts)
	dropped  bool                       // Flag whether the peer dropped off early
}

// timedOut returns if this request timed out.
func (req *stateReq) timedOut() bool {
	return req.response == nil
}

// stateSyncStats is a collection of progress stats to report during a state trie
// sync to RPC requests as well as to display in user logs.
//只用于在用户日志中展示.
type stateSyncStats struct {
	processed  uint64 // Number of state entries processed		//已经处理的state entry.
	duplicate  uint64 // Number of state entries downloaded twice	//下载了两次的state entry.
	unexpected uint64 // Number of non-requested state entries received	//收到的非请求state entry.
	pending    uint64 // Number of still pending state entries		//仍然处于Pending状态(已经下载但是尚未被写入数据库)的state entry.
}

// syncState starts downloading state with the given root hash.
func (d *Downloader) syncState(root common.Hash) *stateSync {
	s := newStateSync(d, root)
	select {
	case d.stateSyncStart <- s:
	case <-d.quitCh:
		s.err = errCancelStateFetch
		close(s.done)
	}
	return s
}

// stateFetcher manages the active state sync and accepts requests
// on its behalf.
func (d *Downloader) stateFetcher() {
	for {
		select {
		case s := <-d.stateSyncStart:	//响应状态同步开始事件.
			for next := s; next != nil; {
				next = d.runStateSync(next)
			}
		case <-d.stateCh:	//对downloader来说,不处理收到的状态数据.
			// Ignore state responses while no sync is running.
		case <-d.quitCh:	//响应退出信号.
			return
		}
	}
}

// runStateSync runs a state synchronisation until it completes or another root
// hash is requested to be switched over to.
//执行状态同步的过程.
//当同步完成或者新的root hash需要同步的时候此函数结束.
func (d *Downloader) runStateSync(s *stateSync) *stateSync {
	var (
		active   = make(map[string]*stateReq) // Currently in-flight requests	尚未处理的同步请求.
		finished []*stateReq                  // Completed or failed requests	已经完成或者失败的同步请求.
		timeout  = make(chan *stateReq)       // Timed out active requests	超时的尚未处理的同步请求.
	)

	//在退出的时候会执行此匿名函数.
	defer func() {
		// Cancel active request timers on exit. Also set peers to idle so they're
		// available for the next sync.
		//对于所有活跃但是尚未处理的请求,取消其定时器并且设置peer为idle状态以便处理下一次同步.
		for _, req := range active {
			req.timer.Stop()	//停止同步请求的计时器.
			req.peer.SetNodeDataIdle(len(req.items))	//设置peer为idele状态以便处理下一次同步.
		}
	}()
	// Run the state sync.
	go s.run()	//执行状态不同.
	defer s.Cancel()	//函数结束的时候退出状态同步.

	//订阅peer断开事件,一旦收到了peer断开事件,那么针对此peer的同步就不再继续了.
	// Listen for peer departure events to cancel assigned tasks
	peerDrop := make(chan *peerConnection, 1024)	//创建接收peer断开事件的通道.
	peerSub := s.d.peers.SubscribePeerDrops(peerDrop)	//订阅peer断开事件.
	defer peerSub.Unsubscribe()	//在函数退出的时候取消订阅.

	for {	//无限循环.
		// Enable sending of the first buffered element if there is one.
		var (
			deliverReq   *stateReq
			deliverReqCh chan *stateReq
		)
		if len(finished) > 0 {
			deliverReq = finished[0]
			deliverReqCh = s.deliver
		}

		select {
		// The stateSync lifecycle:
		case next := <-d.stateSyncStart:
			return next

		case <-s.done:
			return nil

		// Send the next finished request to the current sync:
		case deliverReqCh <- deliverReq:
			finished = append(finished[:0], finished[1:]...)

		// Handle incoming state packs:
		case pack := <-d.stateCh:
			// Discard any data not requested (or previsouly timed out)
			req := active[pack.PeerId()]
			if req == nil {
				log.Debug("Unrequested node data", "peer", pack.PeerId(), "len", pack.Items())
				continue
			}
			// Finalize the request and queue up for processing
			req.timer.Stop()
			req.response = pack.(*statePack).states

			finished = append(finished, req)
			delete(active, pack.PeerId())

			// Handle dropped peer connections:
		case p := <-peerDrop:
			// Skip if no request is currently pending
			req := active[p.id]
			if req == nil {
				continue
			}
			// Finalize the request and queue up for processing
			req.timer.Stop()
			req.dropped = true

			finished = append(finished, req)
			delete(active, p.id)

		// Handle timed-out requests:
		case req := <-timeout:
			// If the peer is already requesting something else, ignore the stale timeout.
			// This can happen when the timeout and the delivery happens simultaneously,
			// causing both pathways to trigger.
			if active[req.peer.id] != req {
				continue
			}
			// Move the timed out data back into the download queue
			finished = append(finished, req)
			delete(active, req.peer.id)

		// Track outgoing state requests:
		case req := <-d.trackStateReq:
			// If an active request already exists for this peer, we have a problem. In
			// theory the trie node schedule must never assign two requests to the same
			// peer. In practive however, a peer might receive a request, disconnect and
			// immediately reconnect before the previous times out. In this case the first
			// request is never honored, alas we must not silently overwrite it, as that
			// causes valid requests to go missing and sync to get stuck.
			if old := active[req.peer.id]; old != nil {
				log.Warn("Busy peer assigned new state fetch", "peer", old.peer.id)

				// Make sure the previous one doesn't get siletly lost
				old.timer.Stop()
				old.dropped = true

				finished = append(finished, old)
			}
			// Start a timer to notify the sync loop if the peer stalled.
			req.timer = time.AfterFunc(req.timeout, func() {
				select {
				case timeout <- req:
				case <-s.done:
					// Prevent leaking of timer goroutines in the unlikely case where a
					// timer is fired just before exiting runStateSync.
				}
			})
			active[req.peer.id] = req
		}
	}
}

// stateSync schedules requests for downloading a particular state trie defined
// by a given state root.
type stateSync struct {
	d *Downloader // Downloader instance to access and manage current peerset	//downloader实例,用来管理当前peerset.

	sched  *trie.TrieSync             // State trie sync scheduler defining the tasks
	keccak hash.Hash                  // Keccak256 hasher to verify deliveries with
	tasks  map[common.Hash]*stateTask // Set of tasks currently queued for retrieval

	numUncommitted   int		//已经下载但是尚未提交的交易数量.
	bytesUncommitted int		//已经下载但是尚未提交的交易字节数.

	deliver    chan *stateReq // Delivery channel multiplexing peer responses
	cancel     chan struct{}  // Channel to signal a termination request
	cancelOnce sync.Once      // Ensures cancel only ever gets called once
	done       chan struct{}  // Channel to signal termination completion		同步完成信号通道.
	err        error          // Any error hit during sync (set before completion)
}

// stateTask represents a single trie node download taks, containing a set of
// peers already attempted retrieval from to detect stalled syncs and abort.
type stateTask struct {
	attempts map[string]struct{}
}

// newStateSync creates a new state trie download scheduler. This method does not
// yet start the sync. The user needs to call run to initiate.
func newStateSync(d *Downloader, root common.Hash) *stateSync {
	return &stateSync{
		d:       d,
		sched:   state.NewStateSync(root, d.stateDB),
		keccak:  sha3.NewKeccak256(),
		tasks:   make(map[common.Hash]*stateTask),
		deliver: make(chan *stateReq),
		cancel:  make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// run starts the task assignment and response processing loop, blocking until
// it finishes, and finally notifying any goroutines waiting for the loop to
// finish.
func (s *stateSync) run() {
	s.err = s.loop()
	close(s.done)
}

// Wait blocks until the sync is done or canceled.
func (s *stateSync) Wait() error {
	<-s.done
	return s.err
}

// Cancel cancels the sync and waits until it has shut down.
func (s *stateSync) Cancel() error {
	s.cancelOnce.Do(func() { close(s.cancel) })
	return s.Wait()
}

// loop is the main event loop of a state trie sync. It it responsible for the
// assignment of new tasks to peers (including sending it to them) as well as
// for the processing of inbound data. Note, that the loop does not directly
// receive data from peers, rather those are buffered up in the downloader and
// pushed here async. The reason is to decouple processing from data receipt
// and timeouts.
//loop是状态树同步的主要循环.
func (s *stateSync) loop() error {
	// Listen for new peer events to assign tasks to them
	newPeer := make(chan *peerConnection, 1024)	//new peer事件接收通道.
	peerSub := s.d.peers.SubscribeNewPeers(newPeer)	//订阅new peer事件.
	defer peerSub.Unsubscribe()	//在函数结束时解除new peer事件的订阅.

	// Keep assigning new tasks until the sync completes or aborts
	//在同步完成或者退出之前持续发送任务给stateSync对象.
	for s.sched.Pending() > 0 {	//如果还有尚未处理的同步请求,那么继续处理,直到所有请求都处理完毕.
		if err := s.commit(false); err != nil {		//如果已经下载但是尚未提交的交易超出了指定的字节数,那么提交这些交易到状态数据库中.
			return err
		}
		s.assignTasks()
		// Tasks assigned, wait for something to happen
		select {
		case <-newPeer:
			// New peer arrived, try to assign it download tasks

		case <-s.cancel:
			return errCancelStateFetch

		case req := <-s.deliver:
			// Response, disconnect or timeout triggered, drop the peer if stalling
			log.Trace("Received node data response", "peer", req.peer.id, "count", len(req.response), "dropped", req.dropped, "timeout", !req.dropped && req.timedOut())
			if len(req.items) <= 2 && !req.dropped && req.timedOut() {
				// 2 items are the minimum requested, if even that times out, we've no use of
				// this peer at the moment.
				log.Warn("Stalling state sync, dropping peer", "peer", req.peer.id)
				s.d.dropPeer(req.peer.id)
			}
			// Process all the received blobs and check for stale delivery
			stale, err := s.process(req)
			if err != nil {
				log.Warn("Node data write error", "err", err)
				return err
			}
			// The the delivery contains requested data, mark the node idle (otherwise it's a timed out delivery)
			if !stale {
				req.peer.SetNodeDataIdle(len(req.response))
			}
		}
	}
	return s.commit(true)	//在此函数退出的时候强制把尚未提交的交易提交到状态数据库中.
}

//提交需要同步的交易到状态数据库中.
//force:
//    true: 强制提交,不管当前有多少未提交的交易,均执行写状态数据库的操作.
//    false: 非强制提交,只有未提交的交易的字节数达到了100K才进行交易的提交,如果小于100K则不进行交易提交.
func (s *stateSync) commit(force bool) error {
	//判断是否需要真正执行提交交易的动作.
	if !force && s.bytesUncommitted < ethdb.IdealBatchSize {
		return nil
	}
	//提交当前尚未提交的所有交易.
	start := time.Now()
	b := s.d.stateDB.NewBatch()		//创建一个批量操作数据库的对象.
	s.sched.Commit(b)				//把已经下载下来的交易首先存储到对象b中.
	if err := b.Write(); err != nil {	//调用b对象的Write()函数批量写入交易到状态数据库中.
		return fmt.Errorf("DB write error: %v", err)
	}
	s.updateStats(s.numUncommitted, 0, 0, time.Since(start))	//在用户日志中打印信息.
	s.numUncommitted = 0	//所有交易均已经提交,把未提交交易数设置为0.
	s.bytesUncommitted = 0	//所有下载的交易均已经提交,把未提交交易字节数设置为0.
	return nil
}

// assignTasks attempts to assing new tasks to all idle peers, either from the
// batch currently being retried, or fetching new data from the trie sync itself.
func (s *stateSync) assignTasks() {
	// Iterate over all idle peers and try to assign them state fetches
	peers, _ := s.d.peers.NodeDataIdlePeers()	//获得所有data idle节点的列表.
	for _, p := range peers {	//处理每个data idle节点.
		// Assign a batch of fetches proportional to the estimated latency/bandwidth
		cap := p.NodeDataCapacity(s.d.requestRTT())	//返回节点的状态数据下载吞吐率.
		req := &stateReq{peer: p, timeout: s.d.requestTTL()}	//封装一个状态请求对象.
		s.fillTasks(cap, req)

		// If the peer was assigned tasks to fetch, send the network request
		if len(req.items) > 0 {
			req.peer.log.Trace("Requesting new batch of data", "type", "state", "count", len(req.items))
			select {
			case s.d.trackStateReq <- req:
				req.peer.FetchNodeData(req.items)
			case <-s.cancel:
			}
		}
	}
}

// fillTasks fills the given request object with a maximum of n state download
// tasks to send to the remote peer.
func (s *stateSync) fillTasks(n int, req *stateReq) {
	// Refill available tasks from the scheduler.
	if len(s.tasks) < n {
		new := s.sched.Missing(n - len(s.tasks))
		for _, hash := range new {
			s.tasks[hash] = &stateTask{make(map[string]struct{})}
		}
	}
	// Find tasks that haven't been tried with the request's peer.
	req.items = make([]common.Hash, 0, n)
	req.tasks = make(map[common.Hash]*stateTask, n)
	for hash, t := range s.tasks {
		// Stop when we've gathered enough requests
		if len(req.items) == n {
			break
		}
		// Skip any requests we've already tried from this peer
		if _, ok := t.attempts[req.peer.id]; ok {
			continue
		}
		// Assign the request to this peer
		t.attempts[req.peer.id] = struct{}{}
		req.items = append(req.items, hash)
		req.tasks[hash] = t
		delete(s.tasks, hash)
	}
}

// process iterates over a batch of delivered state data, injecting each item
// into a running state sync, re-queuing any items that were requested but not
// delivered.
func (s *stateSync) process(req *stateReq) (bool, error) {
	// Collect processing stats and update progress if valid data was received
	duplicate, unexpected := 0, 0

	defer func(start time.Time) {
		if duplicate > 0 || unexpected > 0 {
			s.updateStats(0, duplicate, unexpected, time.Since(start))
		}
	}(time.Now())

	// Iterate over all the delivered data and inject one-by-one into the trie
	progress, stale := false, len(req.response) > 0

	for _, blob := range req.response {
		prog, hash, err := s.processNodeData(blob)
		switch err {
		case nil:
			s.numUncommitted++
			s.bytesUncommitted += len(blob)
			progress = progress || prog
		case trie.ErrNotRequested:
			unexpected++
		case trie.ErrAlreadyProcessed:
			duplicate++
		default:
			return stale, fmt.Errorf("invalid state node %s: %v", hash.String(), err)
		}
		// If the node delivered a requested item, mark the delivery non-stale
		if _, ok := req.tasks[hash]; ok {
			delete(req.tasks, hash)
			stale = false
		}
	}
	// If we're inside the critical section, reset fail counter since we progressed.
	if progress && atomic.LoadUint32(&s.d.fsPivotFails) > 1 {
		log.Trace("Fast-sync progressed, resetting fail counter", "previous", atomic.LoadUint32(&s.d.fsPivotFails))
		atomic.StoreUint32(&s.d.fsPivotFails, 1) // Don't ever reset to 0, as that will unlock the pivot block
	}

	// Put unfulfilled tasks back into the retry queue
	npeers := s.d.peers.Len()
	for hash, task := range req.tasks {
		// If the node did deliver something, missing items may be due to a protocol
		// limit or a previous timeout + delayed delivery. Both cases should permit
		// the node to retry the missing items (to avoid single-peer stalls).
		if len(req.response) > 0 || req.timedOut() {
			delete(task.attempts, req.peer.id)
		}
		// If we've requested the node too many times already, it may be a malicious
		// sync where nobody has the right data. Abort.
		if len(task.attempts) >= npeers {
			return stale, fmt.Errorf("state node %s failed with all peers (%d tries, %d peers)", hash.String(), len(task.attempts), npeers)
		}
		// Missing item, place into the retry queue.
		s.tasks[hash] = task
	}
	return stale, nil
}

// processNodeData tries to inject a trie node data blob delivered from a remote
// peer into the state trie, returning whether anything useful was written or any
// error occurred.
func (s *stateSync) processNodeData(blob []byte) (bool, common.Hash, error) {
	res := trie.SyncResult{Data: blob}
	s.keccak.Reset()
	s.keccak.Write(blob)
	s.keccak.Sum(res.Hash[:0])
	committed, _, err := s.sched.Process([]trie.SyncResult{res})
	return committed, res.Hash, err
}

// updateStats bumps the various state sync progress counters and displays a log
// message for the user to see.
//更新统计状态,只用来在用户日志中打印信息.
//written: 真正写入数据库的交易数.
//duplicate: 重复交易数.
//unexpected: 不应该到来的交易数.
//duration: 从什么时间开始的统计.
func (s *stateSync) updateStats(written, duplicate, unexpected int, duration time.Duration) {
	s.d.syncStatsLock.Lock()	//给同步状态锁加写锁.
	defer s.d.syncStatsLock.Unlock()	//函数结束后解除同步状态锁的写锁状态.

	s.d.syncStatsState.pending = uint64(s.sched.Pending())	//pending状态的state entry.
	s.d.syncStatsState.processed += uint64(written)		//已经处理的state entry.
	s.d.syncStatsState.duplicate += uint64(duplicate)		//下载了两次的state entry.
	s.d.syncStatsState.unexpected += uint64(unexpected)	//不在请求之列的state entry.

	//如果有pending之外状态的state entry,那么在用户日志中打印信息.
	if written > 0 || duplicate > 0 || unexpected > 0 {
		log.Info("Imported new state entries", "count", written, "elapsed", common.PrettyDuration(duration), "processed", s.d.syncStatsState.processed, "pending", s.d.syncStatsState.pending, "retry", len(s.tasks), "duplicate", s.d.syncStatsState.duplicate, "unexpected", s.d.syncStatsState.unexpected)
	}
}
