// Copyright 2016 The go-ethereum Authors
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

package event

import (
	"errors"
	"reflect"
	"sync"
)

//错误信息。
var errBadChannel = errors.New("event: Subscribe argument does not have sendable channel type")

// Feed implements one-to-many subscriptions where the carrier of events is a channel.
// Values sent to a Feed are delivered to all subscribed channels simultaneously.
//
// Feeds can only be used with a single type. The type is determined by the first Send or
// Subscribe operation. Subsequent calls to these methods panic if the type does not
// match.
//

//Feed类型通过通道的方式实现了一对多的发布订阅机制。发送给Feed对象的消息会被同时传递到所有的订阅通道中。
//Feed只能用于单一类型，这个类型决定于第一次发送或者订阅操作中使用的类型，如果后续的操作中使用的类型与第一次操作的类型不一致的话，那么会引起panic。

// The zero value is ready to use.
type Feed struct {
	once      sync.Once        // ensures that init only runs once	//sync.Once对象只会被执行一次.
	sendLock  chan struct{}    // sendLock has a one-element buffer and is empty when held.It protects sendCases.
	removeSub chan interface{} // interrupts Send
	sendCases caseList         // the active set of select cases used by Send

	// The inbox holds newly subscribed channels until they are added to sendCases.
	mu     sync.Mutex	//操作inbox，etype时需要加锁。
	inbox  caseList		//selectCase列表。
	etype  reflect.Type	//反射的数据类型。
	closed bool
}

// This is the index of the first actual subscription channel in sendCases.
// sendCases[0] is a SelectRecv case for the removeSub channel.
const firstSubSendCase = 1

//feed type 错误。
type feedTypeError struct {
	got, want reflect.Type
	op        string
}

//把feed error错误转换成字符串。
func (e feedTypeError) Error() string {
	return "event: wrong type in " + e.op + " got " + e.got.String() + ", want " + e.want.String()
}

//Feed初始化。
func (f *Feed) init() {
	f.removeSub = make(chan interface{})  //removeSub是一个空接口类型的通道，也就是能传递任意类型。
	f.sendLock = make(chan struct{}, 1) //sendLock是一个空struct类型的异步通道，其buffer为1.
	f.sendLock <- struct{}{}	//构造了一个 struct{}类型的对象，并发送到通道sendLock上去。此步骤后sendLock的发送端被阻塞。
	f.sendCases = caseList{{Chan: reflect.ValueOf(f.removeSub)/*此处返回一个空值*/, Dir: reflect.SelectRecv /*接受通道*/}}
}

// Subscribe adds a channel to the feed. Future sends will be delivered on the channel
// until the subscription is canceled. All channels added must have the same element type.
//
// The channel should have ample buffer space to avoid blocking other subscribers.
// Slow subscribers are not dropped.
//向feed增加一个通道. 后续的消息也会广播到这个通道中. 如果不想再接收消息,需要取消调用.
//添加的所有通道必须具有相同的类型， 添加的通道需要注意要有足够的空间以免阻塞其他调用者。
func (f *Feed) Subscribe(channel interface{}) Subscription {
	f.once.Do(f.init)	//初始化函数,只被执行一次.

	chanval := reflect.ValueOf(channel) //获得参数的值（channel的指针）
	chantyp := chanval.Type()	//获得参数的类型（应该是chan类型）
	if chantyp.Kind() != reflect.Chan || chantyp.ChanDir()&reflect.SendDir == 0 { 	//如果传递的参数不是chan或者通道的方向不是SendDir则panic,程序退出。(应该把chan<-传递给SendDir,把<-chan传递给RecvDir)
		panic(errBadChannel)
	}
	//从参数中构造一个订阅对象。
	sub := &feedSub{feed: f, channel: chanval, err: make(chan error, 1)}

	f.mu.Lock()
	defer f.mu.Unlock()
	//feed类型必须和数据类型相同，否则报错。
	if !f.typecheck(chantyp.Elem()) {
		panic(feedTypeError{op: "Subscribe", got: chantyp, want: reflect.ChanOf(reflect.SendDir, f.etype)})
	}
	// Add the select case to the inbox.
	// The next Send will add it to f.sendCases.
	//send类型的selectCase。
	cas := reflect.SelectCase{Dir: reflect.SelectSend, Chan: chanval}
	f.inbox = append(f.inbox, cas)	//添加到feed的selectCase列表中。
	return sub //返回订阅对象。
}

// note: callers must hold f.mu
//判断数据类型和通道类型是否相同，相同返回true，不同返回false.
func (f *Feed) typecheck(typ reflect.Type) bool {
	if f.etype == nil {
		f.etype = typ
		return true
	}
	return f.etype == typ
}

//把一个订阅对象从订阅列表中移出。
func (f *Feed) remove(sub *feedSub) {
	// Delete from inbox first, which covers channels
	// that have not been added to f.sendCases yet.
	ch := sub.channel.Interface()
	f.mu.Lock()
	index := f.inbox.find(ch)
	if index != -1 {
		f.inbox = f.inbox.delete(index)
		f.mu.Unlock()
		return
	}
	f.mu.Unlock()

	select {
	case f.removeSub <- ch:
		// Send will remove the channel from f.sendCases.
	case <-f.sendLock:
		// No Send is in progress, delete the channel now that we have the send lock.
		f.sendCases = f.sendCases.delete(f.sendCases.find(ch))
		f.sendLock <- struct{}{}
	}
}

// Send delivers to all subscribed channels simultaneously.
// It returns the number of subscribers that the value was sent to.
//发送消息给所有订阅对象，返回send成功的sub数量。
func (f *Feed) Send(value interface{}) (nsent int) {
	f.once.Do(f.init)	//只执行一次。
	<-f.sendLock	//允许发送消息。

	// Add new cases from the inbox after taking the send lock.
	f.mu.Lock()
	f.sendCases = append(f.sendCases, f.inbox...)
	f.inbox = nil
	f.mu.Unlock()

	// Set the sent value on all channels.
	rvalue := reflect.ValueOf(value)
	//如果要发送数据的类型与通道的类型不同，那么阻塞发送，报错。
	if !f.typecheck(rvalue.Type()) {
		f.sendLock <- struct{}{}
		panic(feedTypeError{op: "Send", got: rvalue.Type(), want: f.etype})
	}
	for i := firstSubSendCase; i < len(f.sendCases); i++ {
		f.sendCases[i].Send = rvalue	//给每个通道设置要发送的值。
	}

	// Send until all channels except removeSub have been chosen.
	cases := f.sendCases
	for {
		// Fast path: try sending without blocking before adding to the select set.
		// This should usually succeed if subscribers are fast enough and have free
		// buffer space.
		for i := firstSubSendCase; i < len(cases); i++ {
			if cases[i].Chan.TrySend(rvalue) {
				nsent++
				cases = cases.deactivate(i)
				i--
			}
		}
		if len(cases) == firstSubSendCase {
			break
		}
		// Select on all the receivers, waiting for them to unblock.
		chosen, recv, _ := reflect.Select(cases)
		if chosen == 0 /* <-f.removeSub */ {
			index := f.sendCases.find(recv.Interface())
			f.sendCases = f.sendCases.delete(index)
			if index >= 0 && index < len(cases) {
				cases = f.sendCases[:len(cases)-1]
			}
		} else {
			cases = cases.deactivate(chosen)
			nsent++
		}
	}

	// Forget about the sent value and hand off the send lock.
	for i := firstSubSendCase; i < len(f.sendCases); i++ {
		f.sendCases[i].Send = reflect.Value{}
	}
	f.sendLock <- struct{}{}
	return nsent
}

//订阅结构体。
type feedSub struct {
	feed    *Feed	//订阅关联的feed。
	channel reflect.Value	//订阅的channel。
	errOnce sync.Once	//syncOnce只执行一次。
	err     chan error	//err是error类型的channel.
}

//解除订阅。
func (sub *feedSub) Unsubscribe() {
	sub.errOnce.Do(func() {
		sub.feed.remove(sub)
		close(sub.err)	//关闭通道。
	})
}

//返回错误信息对象。
func (sub *feedSub) Err() <-chan error {
	return sub.err
}

type caseList []reflect.SelectCase		//reflect.SelectCase描述了select中的单条case。

//查找通道对应索引。
// find returns the index of a case containing the given channel.
func (cs caseList) find(channel interface{}) int {
	for i, cas := range cs {
		if cas.Chan.Interface() == channel {
			return i
		}
	}
	return -1
}

// delete removes the given case from cs.
//从selectCase列表中删除指定的selectCase。
func (cs caseList) delete(index int) caseList {
	return append(cs[:index], cs[index+1:]...)
}

// deactivate moves the case at index into the non-accessible portion of the cs slice.
func (cs caseList) deactivate(index int) caseList {
	last := len(cs) - 1
	cs[index], cs[last] = cs[last], cs[index]
	return cs[:last]
}

// func (cs caseList) String() string {
//     s := "["
//     for i, cas := range cs {
//             if i != 0 {
//                     s += ", "
//             }
//             switch cas.Dir {
//             case reflect.SelectSend:
//                     s += fmt.Sprintf("%v<-", cas.Chan.Interface())
//             case reflect.SelectRecv:
//                     s += fmt.Sprintf("<-%v", cas.Chan.Interface())
//             }
//     }
//     return s + "]"
// }
