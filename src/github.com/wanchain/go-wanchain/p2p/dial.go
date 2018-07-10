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

package p2p

import (
	"container/heap"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/wanchain/go-wanchain/log"
	"github.com/wanchain/go-wanchain/p2p/discover"
	"github.com/wanchain/go-wanchain/p2p/netutil"
)

const (
	// This is the amount of time spent waiting in between
	// redialing a certain node.
	dialHistoryExpiration = 30 * time.Second

	// Discovery lookups are throttled and can only run
	// once every few seconds.
	lookupInterval = 4 * time.Second

	// If no peers are found for this amount of time, the initial bootnodes are
	// attempted to be connected.
	fallbackInterval = 20 * time.Second

	// Endpoint resolution is throttled with bounded backoff.
	initialResolveDelay = 60 * time.Second
	maxResolveDelay     = time.Hour
)

// NodeDialer is used to connect to nodes in the network, typically by using
// an underlying net.Dialer but also using net.Pipe in tests
//节点拨号接口.
type NodeDialer interface {
	Dial(*discover.Node) (net.Conn, error)	//拨号接口.
}

// TCPDialer implements the NodeDialer interface by using a net.Dialer to
// create TCP connections to nodes in the network
//实现了NodeDialer接口.
type TCPDialer struct {
	*net.Dialer
}

// Dial creates a TCP connection to the node
//在节点之间创建一个tcp连接.
func (t TCPDialer) Dial(dest *discover.Node) (net.Conn, error) {
	addr := &net.TCPAddr{IP: dest.IP, Port: int(dest.TCP)}	//构造地址.
	return t.Dialer.Dial("tcp", addr.String())	//返回一个tcp连接.
}

// dialstate schedules dials and discovery lookups.
// it get's a chance to compute new tasks on every iteration
// of the main loop in Server.run.
type dialstate struct {
	maxDynDials int
	ntab        discoverTable
	netrestrict *netutil.Netlist

	lookupRunning bool
	dialing       map[discover.NodeID]connFlag
	lookupBuf     []*discover.Node // current discovery lookup results
	randomNodes   []*discover.Node // filled from Table
	static        map[discover.NodeID]*dialTask	//存储静态节点的拨号task.
	hist          *dialHistory

	start     time.Time        // time when the dialer was first used	拨号开始时间.
	bootnodes []*discover.Node // default dials when there are no peers	//没有peers时候的默认拨号节点.
}

type discoverTable interface {
	Self() *discover.Node
	Close()
	Resolve(target discover.NodeID) *discover.Node
	Lookup(target discover.NodeID) []*discover.Node
	ReadRandomNodes([]*discover.Node) int
}

// the dial history remembers recent dials.
type dialHistory []pastDial

// pastDial is an entry in the dial history.
type pastDial struct {
	id  discover.NodeID
	exp time.Time
}

//task接口.
type task interface {
	Do(*Server)	//执行task.
}

// A dialTask is generated for each node that is dialed. Its
// fields cannot be accessed while the task is running.
type dialTask struct {
	flags        connFlag
	dest         *discover.Node		//拨号的目的节点(要向哪个node拨号.)
	lastResolved time.Time
	resolveDelay time.Duration
}

// discoverTask runs discovery table operations.
// Only one discoverTask is active at any time.
// discoverTask.Do performs a random lookup.
type discoverTask struct {
	results []*discover.Node
}

// A waitExpireTask is generated if there are no other tasks
// to keep the loop in Server.run ticking.
type waitExpireTask struct {
	time.Duration
}

func newDialState(static []*discover.Node, bootnodes []*discover.Node, ntab discoverTable, maxdyn int, netrestrict *netutil.Netlist) *dialstate {
	s := &dialstate{
		maxDynDials: maxdyn,
		ntab:        ntab,
		netrestrict: netrestrict,
		static:      make(map[discover.NodeID]*dialTask),
		dialing:     make(map[discover.NodeID]connFlag),
		bootnodes:   make([]*discover.Node, len(bootnodes)),
		randomNodes: make([]*discover.Node, maxdyn/2),
		hist:        new(dialHistory),
	}
	copy(s.bootnodes, bootnodes)
	for _, n := range static {
		s.addStatic(n)
	}
	return s
}

func (s *dialstate) addStatic(n *discover.Node) {
	// This overwites the task instead of updating an existing
	// entry, giving users the opportunity to force a resolve operation.
	s.static[n.ID] = &dialTask{flags: staticDialedConn, dest: n}
}

func (s *dialstate) removeStatic(n *discover.Node) {
	// This removes a task so future attempts to connect will not be made.
	delete(s.static, n.ID)
}

//创建新task列表.
//    nRunning: 正在运行的task数.
//    peers: 对端列表.(peers中存储的是已经建立了网络连接的节点,尚未建立连接的节点不会存储在peers中.)
//    now: 时间戳.
//    返回创建好的task列表.
func (s *dialstate) newTasks(nRunning int, peers map[discover.NodeID]*Peer, now time.Time) []task {
	//设置拨号开始时间.
	if s.start == (time.Time{}) {
		s.start = now
	}

	//新创建task列表.
	var newtasks []task

	//匿名函数,添加拨号对象.
	addDial := func(flag connFlag, n *discover.Node) bool {
		//首先检测拨号是否符合规则,如果不符合规则则退出.
		if err := s.checkDial(n, peers); err != nil {
			log.Trace("Skipping dial candidate", "id", n.ID, "addr", &net.TCPAddr{IP: n.IP, Port: int(n.TCP)}, "err", err)
			return false
		}
		s.dialing[n.ID] = flag	//设置连接标志.
		newtasks = append(newtasks, &dialTask{flags: flag, dest: n})	//把新的task添加到newtasks切片中.
		return true
	}

	// Compute number of dynamic dials necessary at this point.
	needDynDials := s.maxDynDials
	for _, p := range peers {
		if p.rw.is(dynDialedConn) {
			needDynDials--
		}
	}
	for _, flag := range s.dialing {
		if flag&dynDialedConn != 0 {
			needDynDials--
		}
	}

	// Expire the dial history on every invocation.
	//每次调用newTasks函数都让当前dialState的历史状态过期.
	s.hist.expire(now)

	// Create dials for static nodes if they are not connected.
	//如果静态节点还没有连接,那么把静态节点添加到newtasks中.
	for id, t := range s.static {
		err := s.checkDial(t.dest, peers)
		switch err {
		case errNotWhitelisted, errSelf:
			log.Warn("Removing static dial candidate", "id", t.dest.ID, "addr", &net.TCPAddr{IP: t.dest.IP, Port: int(t.dest.TCP)}, "err", err)
			delete(s.static, t.dest.ID)
		case nil:
			s.dialing[id] = t.flags
			newtasks = append(newtasks, t)
		}
	}
	// If we don't have any peers whatsoever, try to dial a random bootnode. This
	// scenario is useful for the testnet (and private networks) where the discovery
	// table might be full of mostly bad peers, making it hard to find good ones.
	if len(peers) == 0 && len(s.bootnodes) > 0 && needDynDials > 0 && now.Sub(s.start) > fallbackInterval {
		bootnode := s.bootnodes[0]	//选取第一个bootnode.
		//把已经选中的bootnode移动到列表最后,方便下一次选择的时候直接从第一个bootnode选择.
		s.bootnodes = append(s.bootnodes[:0], s.bootnodes[1:]...)
		s.bootnodes = append(s.bootnodes, bootnode)

		//添加bootnode到newTasks中.
		if addDial(dynDialedConn, bootnode) {
			needDynDials--
		}
	}
	// Use random nodes from the table for half of the necessary
	// dynamic dials.
	randomCandidates := needDynDials / 2
	if randomCandidates > 0 {
		n := s.ntab.ReadRandomNodes(s.randomNodes)
		for i := 0; i < randomCandidates && i < n; i++ {
			if addDial(dynDialedConn, s.randomNodes[i]) {
				needDynDials--
			}
		}
	}
	// Create dynamic dials from random lookup results, removing tried
	// items from the result buffer.
	i := 0
	for ; i < len(s.lookupBuf) && needDynDials > 0; i++ {
		if addDial(dynDialedConn, s.lookupBuf[i]) {
			needDynDials--
		}
	}
	s.lookupBuf = s.lookupBuf[:copy(s.lookupBuf, s.lookupBuf[i:])]
	// Launch a discovery lookup if more candidates are needed.
	if len(s.lookupBuf) < needDynDials && !s.lookupRunning {
		s.lookupRunning = true
		newtasks = append(newtasks, &discoverTask{})
	}

	// Launch a timer to wait for the next node to expire if all
	// candidates have been tried and no task is currently active.
	// This should prevent cases where the dialer logic is not ticked
	// because there are no pending events.
	if nRunning == 0 && len(newtasks) == 0 && s.hist.Len() > 0 {
		t := &waitExpireTask{s.hist.min().exp.Sub(now)}
		newtasks = append(newtasks, t)
	}
	return newtasks
}

var (
	errSelf             = errors.New("is self")
	errAlreadyDialing   = errors.New("already dialing")
	errAlreadyConnected = errors.New("already connected")
	errRecentlyDialed   = errors.New("recently dialed")
	errNotWhitelisted   = errors.New("not contained in netrestrict whitelist")
)

//检测拨号是否合法.
func (s *dialstate) checkDial(n *discover.Node, peers map[discover.NodeID]*Peer) error {
	_, dialing := s.dialing[n.ID]
	switch {
	case dialing:
		return errAlreadyDialing
	case peers[n.ID] != nil:
		return errAlreadyConnected
	case s.ntab != nil && n.ID == s.ntab.Self().ID:
		return errSelf
	case s.netrestrict != nil && !s.netrestrict.Contains(n.IP):
		return errNotWhitelisted
	case s.hist.contains(n.ID):
		return errRecentlyDialed
	}
	return nil
}

func (s *dialstate) taskDone(t task, now time.Time) {
	switch t := t.(type) {
	case *dialTask:
		s.hist.add(t.dest.ID, now.Add(dialHistoryExpiration))
		delete(s.dialing, t.dest.ID)
	case *discoverTask:
		s.lookupRunning = false
		s.lookupBuf = append(s.lookupBuf, t.results...)
	}
}

//拨号task.
func (t *dialTask) Do(srv *Server) {
	if t.dest.Incomplete() {
		if !t.resolve(srv) {
			return
		}
	}
	success := t.dial(srv, t.dest)	//拨号.
	// Try resolving the ID of static nodes if dialing failed.
	if !success && t.flags&staticDialedConn != 0 {
		if t.resolve(srv) {
			t.dial(srv, t.dest)
		}
	}
}

// resolve attempts to find the current endpoint for the destination
// using discovery.
//
// Resolve operations are throttled with backoff to avoid flooding the
// discovery network with useless queries for nodes that don't exist.
// The backoff delay resets when the node is found.
func (t *dialTask) resolve(srv *Server) bool {
	if srv.ntab == nil {
		log.Debug("Can't resolve node", "id", t.dest.ID, "err", "discovery is disabled")
		return false
	}
	if t.resolveDelay == 0 {
		t.resolveDelay = initialResolveDelay
	}
	if time.Since(t.lastResolved) < t.resolveDelay {
		return false
	}
	resolved := srv.ntab.Resolve(t.dest.ID)
	t.lastResolved = time.Now()
	if resolved == nil {
		t.resolveDelay *= 2
		if t.resolveDelay > maxResolveDelay {
			t.resolveDelay = maxResolveDelay
		}
		log.Debug("Resolving node failed", "id", t.dest.ID, "newdelay", t.resolveDelay)
		return false
	}
	// The node was found.
	t.resolveDelay = initialResolveDelay
	t.dest = resolved
	log.Debug("Resolved node", "id", t.dest.ID, "addr", &net.TCPAddr{IP: t.dest.IP, Port: int(t.dest.TCP)})
	return true
}

// dial performs the actual connection attempt.
// dial执行实际的节点连接操作.
func (t *dialTask) dial(srv *Server, dest *discover.Node) bool {
	fd, err := srv.Dialer.Dial(dest)	//建立实际的TCP连接.
	if err != nil {
		log.Trace("Dial error", "task", t, "err", err)
		return false
	}
	mfd := newMeteredConn(fd, false)
	srv.SetupConn(mfd, t.flags, dest)	//执行handshake.
	return true
}

func (t *dialTask) String() string {
	return fmt.Sprintf("%v %x %v:%d", t.flags, t.dest.ID[:8], t.dest.IP, t.dest.TCP)
}

func (t *discoverTask) Do(srv *Server) {
	// newTasks generates a lookup task whenever dynamic dials are
	// necessary. Lookups need to take some time, otherwise the
	// event loop spins too fast.
	next := srv.lastLookup.Add(lookupInterval)
	if now := time.Now(); now.Before(next) {
		time.Sleep(next.Sub(now))
	}
	srv.lastLookup = time.Now()
	var target discover.NodeID
	rand.Read(target[:])
	t.results = srv.ntab.Lookup(target)
}

func (t *discoverTask) String() string {
	s := "discovery lookup"
	if len(t.results) > 0 {
		s += fmt.Sprintf(" (%d results)", len(t.results))
	}
	return s
}

func (t waitExpireTask) Do(*Server) {
	time.Sleep(t.Duration)
}
func (t waitExpireTask) String() string {
	return fmt.Sprintf("wait for dial hist expire (%v)", t.Duration)
}

// Use only these methods to access or modify dialHistory.
func (h dialHistory) min() pastDial {
	return h[0]
}
func (h *dialHistory) add(id discover.NodeID, exp time.Time) {
	heap.Push(h, pastDial{id, exp})
}
func (h dialHistory) contains(id discover.NodeID) bool {
	for _, v := range h {
		if v.id == id {
			return true
		}
	}
	return false
}
func (h *dialHistory) expire(now time.Time) {
	for h.Len() > 0 && h.min().exp.Before(now) {
		heap.Pop(h)
	}
}

// heap.Interface boilerplate
func (h dialHistory) Len() int           { return len(h) }
func (h dialHistory) Less(i, j int) bool { return h[i].exp.Before(h[j].exp) }
func (h dialHistory) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *dialHistory) Push(x interface{}) {
	*h = append(*h, x.(pastDial))
}
func (h *dialHistory) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}
