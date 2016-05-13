package wango

import (
	"io"
	"regexp"
	"strings"
	"sync"

	"github.com/pkg/errors"
	"golang.org/x/net/websocket"
)

// WS is a main struct – WAMP server
type WS struct {
	connections       map[string]*conn
	connectionsLocker sync.RWMutex
	rpcHandlers       map[string]RPCHandler
	rpcRgxHandlers    map[*regexp.Regexp]RPCHandler
	subHandlers       map[string]subHandler
	subscribers       map[string]subscribersMap
	subscribersLocker sync.RWMutex
	openCB            func()
	closeCB           func()
}

// RPCHandler describes func for handling RPC requests
type RPCHandler func(connID string, uri string, args ...interface{}) (interface{}, error)

// SubHandler describes func for handling RPC requests
type SubHandler func(connID string, uri string, args ...interface{}) bool

// PubHandler describes func for handling publish event before sending to subscribers
type PubHandler func(uri string, event interface{}, extra interface{}) (bool, interface{})

type subHandler struct {
	subHandler SubHandler
	pubHandler PubHandler
}
type subscribersMap map[string]bool

type conn struct {
	id         string
	connection *websocket.Conn
	extra      interface{}
	sendChan   chan interface{}
}

// New creates new WS struct and returns pointer to it
func New() *WS {
	server := new(WS)
	server.connections = map[string]*conn{}
	server.rpcHandlers = map[string]RPCHandler{}
	server.rpcRgxHandlers = map[*regexp.Regexp]RPCHandler{}
	server.subHandlers = map[string]subHandler{}
	server.subscribers = map[string]subscribersMap{}
	return server
}

// RegisterRPCHandler registers RPC handler function for provided URI
func (server *WS) RegisterRPCHandler(_uri interface{}, fn RPCHandler) error {
	switch _uri.(type) {
	case string:
		uri := _uri.(string)
		if _, ok := server.rpcHandlers[uri]; ok {
			return errors.Wrap(ErrHandlerAlreadyRegistered, "when registering string rpcHandler")
		}
		server.rpcHandlers[uri] = fn
	case *regexp.Regexp:
		rgx := _uri.(*regexp.Regexp)
		for k := range server.rpcRgxHandlers {
			if k.String() == rgx.String() {
				return errors.Wrap(ErrHandlerAlreadyRegistered, "when registering rgx rpcHandler")
			}
		}
		server.rpcRgxHandlers[rgx] = fn
	}

	return nil
}

// RegisterSubHandler registers subscription handler function for provided URI
func (server *WS) RegisterSubHandler(uri string, fnSub SubHandler, fnPub PubHandler) error {
	if _, ok := server.subHandlers[uri]; ok {
		return errors.Wrap(ErrHandlerAlreadyRegistered, "when registering subHandler")
	}

	server.subHandlers[uri] = subHandler{
		subHandler: fnSub,
		pubHandler: fnPub,
	}
	return nil
}

// Publish used for publish event
func (server *WS) Publish(uri string, event interface{}) {
	var pubHandler PubHandler
	handler, ok := server.subHandlers[uri]
	if ok {
		pubHandler = handler.pubHandler
	}
	server.subscribersLocker.RLock()
	subscribers, ok := server.subscribers[uri]
	if !ok {
		server.subscribersLocker.RUnlock()
		return
	}
	if len(subscribers) == 0 {
		server.subscribersLocker.RUnlock()
		return
	}
	// need to copy ids to prevent long locking
	subscriberIds := make([]string, len(subscribers))
	i := 0
	for id := range subscribers {
		subscriberIds[i] = id
		i++
	}
	server.subscribersLocker.RUnlock()
	for _, id := range subscriberIds {
		c, err := server.getConnection(id)
		if err != nil {
			println("Connection not found", err)
			continue
		}

		var response []byte
		if pubHandler != nil {
			allow, modifiedEvent := pubHandler(uri, event, c.extra)
			if !allow {
				// not allowed to send
				continue
			}
			response, _ = createMessage(msgEvent, uri, modifiedEvent)
		} else {
			response, _ = createMessage(msgEvent, uri, event)
		}
		c.send(response)
	}
}

// WampHandler handles every *websocket.Conn connection
// If extra data provided, it will kept in connection and will pass to rpc/pub/sub handlers
func (server *WS) WampHandler(ws *websocket.Conn, extra interface{}) {
	c := server.addConnection(ws, extra)
	defer server.deleteConnection(c.id)

	go c.sender()

	server.receive(c)
}

func (server *WS) receive(c *conn) {
	defer c.connection.Close()
	var data string
	for {
		err := websocket.Message.Receive(c.connection, &data)
		if err != nil {
			if err != io.EOF {
				// Error receiving message
			}
			break
		}
		msgType, msg, err := parseMessage(data)
		if err != nil {
			// error parsing!!!
			println("Error:", err.Error())
		}
		switch msgType {
		case msgPrefix:
		// not implemented
		case msgCall:
			server.handleRPCCall(c, msg)
		case msgCallResult:
		case msgCallError:
		case msgSubscribe:
			server.handleSubscribe(c, msg)
		case msgUnsubscribe:
			server.handleUnSubscribe(c, msg)
		case msgPublish:
			server.handlePublish(c, msg)
		case msgEvent:
		case msgSubscribed:
		// not implemented
		case msgHeartbeat:
			server.handleHeartbeat(c, msg, data)
		}
	}
}

func (server *WS) handleRPCCall(c *conn, msg []interface{}) {
	rpcMessage, err := parseWampMessage(msgCall, msg)
	if err != nil {
		println("Can't parse rpc message", err.Error())
		return
	}

	uri := rpcMessage.URI
	handler, ok := server.rpcHandlers[uri]
	if !ok {
		var rgx *regexp.Regexp
		for rgx, handler = range server.rpcRgxHandlers {
			if rgx.MatchString(uri) {
				ok = true
				break
			}
		}
	}

	if ok {
		res, err := handler(c.id, uri, rpcMessage.Args...)
		if err != nil {
			response, _ := createMessage(msgCallError, rpcMessage.CallID, createError(err))
			// TODO: error handling
			c.send(response)
			return
		}
		response, _ := createMessage(msgCallResult, rpcMessage.CallID, res)
		c.send(response)
		return
	}

	response, _ := createMessage(msgCallError, rpcMessage.CallID, createError(ErrRPCNotRegistered))
	c.send(response)
}

func (server *WS) handleSubscribe(c *conn, msg []interface{}) {
	subMessage, err := parseWampMessage(msgSubscribe, msg)
	if err != nil {
		println("Can't parse rpc message", err.Error())
		return
	}

	_uri := subMessage.URI
	server.subscribersLocker.Lock()
	defer server.subscribersLocker.Unlock()
	for uri, handler := range server.subHandlers {
		if strings.HasPrefix(_uri, uri) {
			if handler.subHandler(c.id, _uri, subMessage.Args...) {
				if _, ok := server.subscribers[_uri]; !ok {
					server.subscribers[_uri] = subscribersMap{}
				}
				server.subscribers[_uri][c.id] = subscriberExists
				response, _ := createMessage(msgSubscribed, _uri)
				go c.send(response)
				return
			}
			response, _ := createMessage(msgSubscribeError, _uri, createError(ErrForbidden))
			go c.send(response)
			return
		}
	}
	response, _ := createMessage(msgSubscribeError, _uri, createError(ErrSubURINotRegistered))
	go c.send(response)
}

func (server *WS) handleUnSubscribe(c *conn, msg []interface{}) {
	unsubMessage, err := parseWampMessage(msgUnsubscribe, msg)
	if err != nil {
		println("Can't parse rpc message", err.Error())
		return
	}

	_uri := unsubMessage.URI
	server.subscribersLocker.Lock()
	defer server.subscribersLocker.Unlock()
	for uri, subscribers := range server.subscribers {
		if uri == _uri {
			if _, ok := subscribers[c.id]; ok {
				delete(subscribers, c.id)
				response, _ := createMessage(msgUnsubscribed, _uri)
				go c.send(response)
				return
			}
			response, _ := createMessage(msgUnSubscribeError, _uri, createError(ErrNotSubscribes))
			go c.send(response)
			return
		}
	}
	response, _ := createMessage(msgUnSubscribeError, _uri, createError(ErrSubURINotRegistered))
	go c.send(response)
}

func (server *WS) handleHeartbeat(c *conn, msg []interface{}, data string) {
	c.send(data)
}

func (server *WS) handlePublish(c *conn, msg []interface{}) {
	pubMessage, err := parseWampMessage(msgUnsubscribe, msg)
	if err != nil {
		println("Can't parse publish message", err.Error())
		return
	}
	var event interface{}
	if len(pubMessage.Args) > 0 {
		event = pubMessage.Args[0]
	}
	server.Publish(pubMessage.URI, event)
}

func (server *WS) addConnection(ws *websocket.Conn, extra interface{}) *conn {
	cn := new(conn)
	cn.connection = ws
	cn.id = newUUIDv4()
	cn.extra = extra
	cn.sendChan = make(chan interface{}, sendChanBufferSize)
	server.connectionsLocker.Lock()
	defer server.connectionsLocker.Unlock()
	server.connections[cn.id] = cn

	return cn
}

func (server *WS) getConnection(id string) (*conn, error) {
	server.connectionsLocker.RLock()
	defer server.connectionsLocker.RUnlock()
	cn, ok := server.connections[id]
	if !ok {
		return nil, errors.New("NOT FOUND")
	}

	return cn, nil
}

func (server *WS) deleteConnection(id string) {
	server.connectionsLocker.Lock()
	defer server.connectionsLocker.Unlock()
	server.subscribersLocker.Lock()
	defer server.subscribersLocker.Unlock()
	delete(server.connections, id)
	for _, subscribers := range server.subscribers {
		delete(subscribers, id)
	}
}

func (c *conn) send(msg interface{}) {
	c.sendChan <- msg
}

func (c *conn) sender() {
	for msg := range c.sendChan {
		err := websocket.Message.Send(c.connection, msg)
		if err != nil {
			println("Error when send message", err)
		}
	}
}
