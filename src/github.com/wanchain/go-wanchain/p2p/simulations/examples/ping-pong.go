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

package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/wanchain/go-wanchain/log"
	"github.com/wanchain/go-wanchain/node"
	"github.com/wanchain/go-wanchain/p2p"
	"github.com/wanchain/go-wanchain/p2p/discover"
	"github.com/wanchain/go-wanchain/p2p/simulations"
	"github.com/wanchain/go-wanchain/p2p/simulations/adapters"
	"github.com/wanchain/go-wanchain/rpc"
)

//方针适配器的类型, 默认是sim.(sim | exec | docker)
var adapterType = flag.String("adapter", "sim", `node adapter to use (one of "sim", "exec" or "docker")`)

// main() starts a simulation network which contains nodes running a simple
// ping-pong protocol
func main() {
	flag.Parse()	//命令行参数转换.

	// set the log level to Trace
	log.Root().SetHandler(log.LvlFilterHandler(log.LvlTrace, log.StreamHandler(os.Stderr, log.TerminalFormat(false))))

	// register a single ping-pong service
	services := map[string]adapters.ServiceFunc{
		"ping-pong": func(ctx *adapters.ServiceContext) (node.Service, error) {
			return newPingPongService(ctx.Config.ID), nil	//返回ping-pong服务对象.
		},
	}
	adapters.RegisterServices(services)		//注册服务.

	// create the NodeAdapter
	var adapter adapters.NodeAdapter

	switch *adapterType {

	case "sim":	//仿真适配器.
		log.Info("using sim adapter")
		adapter = adapters.NewSimAdapter(services)

	case "exec":	//exec适配器.
		tmpdir, err := ioutil.TempDir("", "p2p-example")
		if err != nil {
			log.Crit("error creating temp dir", "err", err)
		}
		defer os.RemoveAll(tmpdir)
		log.Info("using exec adapter", "tmpdir", tmpdir)
		adapter = adapters.NewExecAdapter(tmpdir)

	case "docker":		//docker适配器.
		log.Info("using docker adapter")
		var err error
		adapter, err = adapters.NewDockerAdapter()
		if err != nil {
			log.Crit("error creating docker adapter", "err", err)
		}

	default:	//默认值.
		log.Crit(fmt.Sprintf("unknown node adapter %q", *adapterType))
	}

	//启动http api.
	// start the HTTP API
	log.Info("starting simulation server on 0.0.0.0:8888...")
	network := simulations.NewNetwork(adapter, &simulations.NetworkConfig{
		DefaultService: "ping-pong",
	})
	//ListenAndServe():监听端口8888,在已经建立的连接上调用simulations.NewServer(network)创建的对象来响应请求.
	if err := http.ListenAndServe(":8888", simulations.NewServer(network)); err != nil {
		log.Crit("error starting simulation server", "err", err)
	}
}

// pingPongService runs a ping-pong protocol between nodes where each node
// sends a ping to all its connected peers every 10s and receives a pong in
// return
// pingPongService在节点之间运行ping-pong协议.每个节点发送一个ping到所有与之连接的节点,然后等待节点的pong响应.
type pingPongService struct {
	id       discover.NodeID	//节点id.
	log      log.Logger			//日志对象.
	received int64				//消息接收计数.
}

//创建一个pingpong服务,
//id: 节点ID.
//返回: 服务对象指针.
func newPingPongService(id discover.NodeID) *pingPongService {
	return &pingPongService{					//pingpong服务对象.
		id:  id,								//节点ID.
		log: log.New("node.id", id),		//日志对象.
	}
}

//返回p2p子协议结构体.
func (p *pingPongService) Protocols() []p2p.Protocol {
	return []p2p.Protocol{{	//返回p2p子协议结构体.
		Name:     "ping-pong",		//协议名称.
		Version:  1,	//协议版本号.
		Length:   2,	//协议消息码长度.
		Run:      p.Run,	//协议运行函数.
		NodeInfo: p.Info,	//节点上关于此协议的信息.
	}}
}

//返回此协议支持的所有rpc apis.
func (p *pingPongService) APIs() []rpc.API {
	return nil
}

//启动service.
func (p *pingPongService) Start(server *p2p.Server) error {
	p.log.Info("ping-pong service starting")
	return nil
}

//停止service.
func (p *pingPongService) Stop() error {
	p.log.Info("ping-pong service stopping")
	return nil
}

//返回node上service对应的Info.
func (p *pingPongService) Info() interface{} {
	return struct {
		Received int64 `json:"received"`
	}{
		atomic.LoadInt64(&p.received),
	}
}

//消息代码.
const (
	pingMsgCode = iota	//ping msg.
	pongMsgCode			//pong msg.
)

// Run implements the ping-pong protocol which sends ping messages to the peer
// at 10s intervals, and responds to pings with pong messages.
// service运行函数.
func (p *pingPongService) Run(peer *p2p.Peer, rw p2p.MsgReadWriter) error {
	log := p.log.New("peer.id", peer.ID())

	errC := make(chan error)	//创建错误信息通道.
	go func() {	//匿名函数.
		for range time.Tick(10 * time.Second) {	//每隔10发送一次消息.
			log.Info("sending ping")
			if err := p2p.Send(rw, pingMsgCode, "PING"); err != nil {	//发送消息.
				errC <- err
				return
			}
		}
	}()
	go func() {	//匿名函数.
		for {
			msg, err := rw.ReadMsg()	//读取消息.
			if err != nil {
				errC <- err
				return
			}
			payload, err := ioutil.ReadAll(msg.Payload)	//读取消息.
			if err != nil {
				errC <- err
				return
			}
			log.Info("received message", "msg.code", msg.Code, "msg.payload", string(payload))
			atomic.AddInt64(&p.received, 1)	//消息计数加1.
			if msg.Code == pingMsgCode {	//如果是ping消息,那么返回pong消息.
				log.Info("sending pong")
				go p2p.Send(rw, pongMsgCode, "PONG")	//启动另外一个协程发送pong消息,这样不会阻塞ping消息的接收,从而加快了整个系统的处理速度.
			}
		}
	}()
	return <-errC
}
