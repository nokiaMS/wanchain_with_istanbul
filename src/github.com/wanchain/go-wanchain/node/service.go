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

package node

import (
        "crypto/ecdsa"
	"reflect"

	"github.com/wanchain/go-wanchain/accounts"
	"github.com/wanchain/go-wanchain/ethdb"
	"github.com/wanchain/go-wanchain/event"
	"github.com/wanchain/go-wanchain/p2p"
	"github.com/wanchain/go-wanchain/rpc"
)

// ServiceContext is a collection of service independent options inherited from
// the protocol stack, that is passed to all constructors to be optionally used;
// as well as utility methods to operate on the service environment.
type ServiceContext struct {
	config         *Config
	services       map[reflect.Type]Service // Index of the already constructed services
	EventMux       *event.TypeMux           // Event multiplexer used for decoupled notifications
	AccountManager *accounts.Manager        // Account manager created by the node.
}

// OpenDatabase opens an existing database with the given name (or creates one
// if no previous can be found) from within the node's data directory. If the
// node is an ephemeral one, a memory database is returned.
//打开名字为name的数据库,如果不存在则创建.
func (ctx *ServiceContext) OpenDatabase(name string, cache int, handles int) (ethdb.Database, error) {
	if ctx.config.DataDir == "" {	//如果数据文件夹配置为空,说明是一个临时节点,那么就创建内存数据库.
		return ethdb.NewMemDatabase()
	}
	db, err := ethdb.NewLDBDatabase(ctx.config.resolvePath(name), cache, handles)
	if err != nil {
		return nil, err
	}
	return db, nil
}

// ResolvePath resolves a user path into the data directory if that was relative
// and if the user actually uses persistent storage. It will return an empty string
// for emphemeral storage and the user's own input for absolute paths.
func (ctx *ServiceContext) ResolvePath(path string) string {
	return ctx.config.resolvePath(path)
}

// Service retrieves a currently running service registered of a specific type.
func (ctx *ServiceContext) Service(service interface{}) error {
	element := reflect.ValueOf(service).Elem()
	if running, ok := ctx.services[element.Type()]; ok {
		element.Set(reflect.ValueOf(running))
		return nil
	}
	return ErrServiceUnknown
}

// NodeKey returns node key from config
func (ctx *ServiceContext) NodeKey() *ecdsa.PrivateKey {
	return ctx.config.NodeKey()
}

// ServiceConstructor is the function signature of the constructors needed to be
// registered for service instantiation.
//ServiceConstructor即函数指针.
type ServiceConstructor func(ctx *ServiceContext) (Service, error)

// Service is an individual protocol that can be registered into a node.		//service代表一个注册在节点上,运行在p2p层之上的子协议.
//
// Notes:
//
// • Service life-cycle management is delegated to the node. The service is allowed to
// initialize itself upon creation, but no goroutines should be spun up outside of the
// Start method.
//
// • Restart logic is not required as the node will create a fresh instance
// every time a service is started.
type Service interface {	//service接口, 一个service代表一个注册在节点上,运行在p2p层之上的子协议.
	// Protocols retrieves the P2P protocols the service wishes to start.
	Protocols() []p2p.Protocol	//返回这个service希望启动的p2p子协议对象.有可能返回多个希望启动的p2p子协议.

	// APIs retrieves the list of RPC descriptors the service provides
	APIs() []rpc.API	//APIs()返回service提供的rpc apis.

	// Start is called after all services have been constructed and the networking
	// layer was also initialized to spawn any goroutines required by the service.
	Start(server *p2p.Server) error		//p2p启动后,而且p2p之上的所有services都被构建之后,Start()函数会被调用来启动本service.

	// Stop terminates all goroutines belonging to the service, blocking until they
	// are all terminated.
	Stop() error    //停止service.
}
