// Bidirectional RPC with JSON messages.
//
// Uses net/rpc, is inspired by net/rpc/jsonrpc, but does more than
// either:
//
// - fully bidirectional: server can call RPCs on the client
// - incoming messages with seq 0 are "untagged" and will not
//   be responded to
//
// This allows one to do RPC over websockets without sacrifing what
// they are good for: sending immediate notifications.
//
// While this is intended for websockets, any io.ReadWriteCloser will
// do.
package jsonrpc

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"sync"

	"github.com/gorilla/websocket"
)

type rpcCall struct {
	ServiceMethod string        // The name of the service and method to call.
	Args          interface{}   // The argument to the function (*struct).
	Reply         interface{}   // The reply from the function (*struct).
	Error         error         // After completion, the error status.
	Done          chan *rpcCall // Receives *Call when Go is complete.

}

// Message is the on-wire description of a method call or result.
//
// Examples:
//
//   {"id":"1","fn":"Arith.Add","args":{"A":1,"B":1}}
//   {"id":"1","result":{"C":2}}
//
// or
//
//   {"id":"1","error":{"msg":"Math is hard, let's go shopping"}}
type Message struct {
	// 0 or omitted for untagged request
	ID *uint64 `json:"id,uint64,omitempty"`

	// Name of the function to call. If set, this is a request; if
	// unset, this is a response.
	Func string `json:"method,omitempty"`

	// Arguments for the RPC call. Only valid for a request.
	Args interface{} `json:"params,omitempty"`

	// Result of the function call. A response will always have
	// either Result or Error set. Only valid for a response.
	Result interface{} `json:"result,omitempty"`

	// Information on how the call failed. Only valid for a
	// response. Must be present if Result is omitted.
	Error *Error `json:"error,omitempty"`
}

type anyMessage struct {
	ID     *uint64         `json:"id,uint64,omitempty"`
	Func   string          `json:"method,omitempty"`
	Args   json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error"`
}

// Error is the on-wire description of an error that occurred while
// serving the method call.
type Error struct {
	Msg string `json:"msg,omitempty"`

	Code int64 `json:"code"`

	Data json.RawMessage `json:"data,omitempty"`
}

func (e Error) Error() string {
	return fmt.Sprintf("jsonrpc: code=%d, name=%s, data=%s", e.Code, e.Msg, e.Data)
}

var (
	noMethodErr = Error{
		Code: -32601,
		Msg:  "NoMethodError",
		Data: []byte("No such function. "),
	}
	invalidParamsErr = Error{
		Code: -32602,
		Msg:  "invalidParamsErr",
		Data: []byte("Invalid method parameter(s). "),
	}
	parseError = Error{
		Code: -32700,
		Msg:  "ParseError",
		Data: []byte("Invalid JSON was received. "),
	}
	serverError = Error{
		Code: -32000,
		Msg:  "ServerError",
		Data: []byte("Error from called method. "),
	}
)

type function struct {
	receiver reflect.Value
	method   reflect.Method
	args     reflect.Type
}

// Registry is a collection of services have methods that can be called remotely.
// Each method has a name in the format SERVICE.METHOD.
//
// A single Registry is intended to be used with multiple Endpoints.
// This separation exists as registering services can be a slow
// operation.
type Registry struct {
	// protects services
	mu        sync.RWMutex
	functions map[string]*function
}

func getRPCMethodsOfType(object interface{}) ([]*function, error) {
	var fns []*function

	type_ := reflect.TypeOf(object)

	for i := 0; i < type_.NumMethod(); i++ {
		method := type_.Method(i)

		if method.PkgPath != "" {
			// skip unexported method
			continue
		}
		if method.Type.NumIn() < 1 {
			return nil, fmt.Errorf("ws_rpc.RegisterService: method %T.%s is missing request argument", object, method.Name)
		}

		// 0 return is for notify
		if method.Type.NumOut() != 0 {
			if method.Type.NumOut() != 2 {
				return nil, fmt.Errorf("ws_rpc.RegisterService: method %T.%s should return both reply & error for call or omit both for notify", object, method.Name)
			}
			var tmp error
			if method.Type.Out(1) != reflect.TypeOf(&tmp).Elem() {
				return nil, fmt.Errorf("ws_rpc.RegisterService: method %T.%s must return error", object, method.Name)
			}
		}

		
		fn := &function{
			receiver: reflect.ValueOf(object),
			method:   method,
			args:     method.Type.In(1),
		}
		fns = append(fns, fn)
	}

	if len(fns) == 0 {
		return nil, fmt.Errorf("ws_rpc.RegisterService: type %T has no exported methods of suitable type", object)
	}
	return fns, nil
}

// RegisterService registers all exported methods of service, allowing
// them to be called remotely. The name of the methods will be of the
// format SERVICE.METHOD, where SERVICE is the type name or the object
// passed in, and METHOD is the name of each method.
//
// The methods are expect to have at least two arguments, referred to
// as args and reply. Reply should be a pointer type, and the method
// should fill it with the result. The types used are limited only by
// the codec needing to be able to marshal them for transport. For
// examples, for wetsock the args and reply must marshal to JSON.
//
// Rest of the arguments are filled on best-effort basis, if their
// types are known to ws_rpc and the codec in use.
//
// The methods should have return type error.
func (r *Registry) RegisterService(object interface{}) *Registry {
	methods, err := getRPCMethodsOfType(object)
	if err != nil {
		// programmer error
		panic(err)
	}

	serviceName := reflect.Indirect(reflect.ValueOf(object)).Type().Name()

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, fn := range methods {
		name := serviceName + "." + fn.method.Name
		r.functions[name] = fn
	}

	return r
}

// NewRegistry creates a new Registry.
func NewRegistry() *Registry {
	r := &Registry{}
	r.functions = make(map[string]*function)
	return r
}

// Endpoint manages the state for one connection (via a Codec) and the
// pending calls on it, both incoming and outgoing.
type Endpoint struct {
	mu   sync.Mutex
	conn *websocket.Conn

	client struct {
		// protects seq and pending
		mutex   sync.Mutex
		seq     uint64
		pending map[uint64]*rpcCall
		notify  chan string
	}

	server struct {
		registry *Registry
		running  sync.WaitGroup
	}
}

// Dummy registry with no functions registered.
var dummyRegistry = NewRegistry()

// NewEndpoint creates a new endpoint that uses codec to talk to a
// peer. To actually process messages, call endpoint.Serve; this is
// done so you can capture errors. Registry can be nil to serve no
// callables from this peer.
func NewEndpoint(conn *websocket.Conn, registry *Registry) *Endpoint {
	if registry == nil {
		registry = dummyRegistry
	}
	e := &Endpoint{}
	e.conn = conn
	e.server.registry = registry
	e.client.pending = make(map[uint64]*rpcCall)
	e.client.notify = make(chan string, 1000)
	return e
}

func NewClient(urlStr string, header http.Header) (*Endpoint, error) {
	conn, _, err := websocket.DefaultDialer.Dial(urlStr, header)
	if err != nil {
		return nil, err
	}
	e := &Endpoint{}
	e.conn = conn
	e.client.pending = make(map[uint64]*rpcCall)
	e.client.notify = make(chan string, 1000)
	go e.Serve()
	return e, nil
}

func NewServer(conn *websocket.Conn, registry *Registry) *Endpoint {
	if registry == nil {
		registry = dummyRegistry
	}
	e := &Endpoint{}
	e.conn = conn
	e.server.registry = registry
	return e
}

func (e *Endpoint) serveRequest(msg *Message) error {
	e.server.registry.mu.RLock()
	fn := e.server.registry.functions[msg.Func]
	e.server.registry.mu.RUnlock()
	if fn == nil {
		rpcErr := noMethodErr
		rpcErr.Data = append([]byte(rpcErr.Data)[:], []byte("method="+msg.Func)...)
		msg.Error = &rpcErr
		msg.Func = ""
		msg.Args = nil
		msg.Result = nil
		err := e.send(msg)
		if err != nil {
			// well, we can't report the problem to the client...
			return err
		}
		return nil
	}

	e.server.running.Add(1)
	go func(fn *function, msg *Message) {
		defer e.server.running.Done()
		e.call(fn, msg)
	}(fn, msg)
	return nil
}

func (e *Endpoint) serveResponse(msg *Message) error {
	e.client.mutex.Lock()
	call, found := e.client.pending[*msg.ID]
	delete(e.client.pending, *msg.ID)
	e.client.mutex.Unlock()

	if !found {
		return fmt.Errorf("Server responded with unknown seq %v", msg.ID)
	}

	if msg.Error == nil {
		if call.Reply != nil {
			raw := msg.Result.(json.RawMessage)
			if raw == nil {
				log.Printf("Received msgID=%d with neither 'result' or 'error'. "+
					"Likely service method is for 'notify' but is called as 'request'\n", *msg.ID)
			} else {
				var err error
				bRaw, _ := json.Marshal(raw)
				if (string(bRaw))[0]== '[' {
					bMsg, _ := json.Marshal(msg)
					err = json.Unmarshal(bMsg, call.Reply)
				}else {
					err = json.Unmarshal(raw, call.Reply)
				}
				if err != nil {
					rpcErr := parseError
					rpcErr.Data = append([]byte(rpcErr.Data)[:], []byte(err.Error())...)
					call.Error = &rpcErr
				}
			}
		}
	} else {
		call.Error = msg.Error
	}

	// notify the caller, but never block
	select {
	case call.Done <- call:
	default:
	}

	return nil
}

func (e *Endpoint) serveNotify(msg *Message) error {

	bMsg, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	e.client.notify <- string(bMsg)

	return nil
}

func (e *Endpoint) ReadNotify(msgChan chan string) {

	for	{
		select {
		case msg := <- e.client.notify:
			{
				msgChan <- msg
			}

		}
	}

}

// Serve messages from this connection. Serve blocks, serving the
// connection until the client disconnects, or there is an error.
func (e *Endpoint) Serve() error {
	defer e.conn.Close()
	defer e.server.running.Wait()
	for {
		var anyMsg anyMessage
		var msg Message
		err := e.conn.ReadJSON(&anyMsg)
		if err != nil {
			return err
		}

		msg.ID = anyMsg.ID
		msg.Func = anyMsg.Func
		msg.Args = anyMsg.Args
		msg.Result = anyMsg.Result
		msg.Error = anyMsg.Error

		switch {
		case msg.Func != "" && msg.ID != nil:
			err = e.serveRequest(&msg)
		case msg.Func == "" && msg.ID != nil:
			err = e.serveResponse(&msg)
		case msg.Func != "" && msg.ID == nil:
			err = e.serveNotify(&msg)
		default:
			// ignore responses from notifications
		}

		if msg.Func != "" {
		} else {
		}
		if err != nil {
			return err
		}
	}
}

func (e *Endpoint) Close() error {
	return e.conn.Close()
}

func (e *Endpoint) send(msg *Message) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.conn.WriteJSON(msg)
}

func (e *Endpoint) fillArgs(argslist []reflect.Value) {
	for i := 0; i < len(argslist); i++ {
		switch argslist[i].Interface().(type) {
		case *websocket.Conn:
			argslist[i] = reflect.ValueOf(e.conn)
		}
	}
}

func (e *Endpoint) call(fn *function, msg *Message) {
	var args reflect.Value
	if fn.args.Kind() == reflect.Ptr {
		args = reflect.New(fn.args.Elem())
	} else {
		args = reflect.New(fn.args)
	}

	raw := msg.Args.(json.RawMessage)
	if raw != nil {

		err := json.Unmarshal(raw, args.Interface())

		if err != nil {
			rpcErr := invalidParamsErr
			rpcErr.Data = append([]byte(rpcErr.Data)[:], []byte(err.Error())...)
			msg.Error = &rpcErr
			msg.Func = ""
			msg.Args = nil
			msg.Result = nil
			err = e.send(msg)
			if err != nil {
				// well, we can't report the problem to the client...
				e.conn.Close()
				return
			}
			return
		}

	}
	if fn.args.Kind() != reflect.Ptr {
		args = args.Elem()
	}

	numArgs := fn.method.Type.NumIn()
	argslist := make([]reflect.Value, numArgs, numArgs)

	argslist[0] = fn.receiver
	argslist[1] = args

	if numArgs > 2 {
		for i := 2; i < numArgs; i++ {
			argslist[i] = reflect.Zero(fn.method.Type.In(i))
		}
		// first fill what we can
		e.fillArgs(argslist[2:])

	}

	var reply interface{}
	retVal := fn.method.Func.Call(argslist)

	if len(retVal) == 2 {
		reply = retVal[0].Interface()
		errIn := retVal[1].Interface()
		if errIn != nil {
			err := errIn.(error)
			rpcErr := serverError
			rpcErr.Data = append([]byte(rpcErr.Data)[:], []byte(err.Error())...)
			msg.Error = &rpcErr
			msg.Func = ""
			msg.Args = nil
			msg.Result = nil
			err = e.send(msg)
			if err != nil {
				// well, we can't report the problem to the client...
				e.conn.Close()
				return
			}
			return
		}
	}

	msg.Error = nil
	msg.Func = ""
	msg.Args = nil
	msg.Result = reply

	err := e.send(msg)
	if err != nil {
		// well, we can't report the problem to the client...
		e.conn.Close()
		return
	}
}

// Go invokes the function asynchronously. See net/rpc Client.Go.
func (e *Endpoint) Go(function string, args interface{}, reply interface{}) *rpcCall {
	call := &rpcCall{}
	call.ServiceMethod = function
	call.Args = args
	call.Reply = reply
	call.Done = make(chan *rpcCall, 1)

	msg := &Message{
		Func: function,
		Args: args,
	}

	e.client.mutex.Lock()
	e.client.seq++
	id := e.client.seq
	msg.ID = &id
	e.client.pending[id] = call
	e.client.mutex.Unlock()

	// put sending in a goroutine so a malicious client that
	// refuses to read cannot ever make a .Go call block
	go e.send(msg)
	return call
}

// Call invokes the named function, waits for it to complete, and
// returns its error status. See net/rpc Client.Call
func (e *Endpoint) Call(function string, args interface{}, reply interface{}) error {
	call := <-e.Go(function, args, reply).Done
	return call.Error
}

// Notify invokes the named function & returns immeidately. Errors if any
// during invocation is logged to stdout
func (e *Endpoint) Notify(function string, args interface{}) {
	msg := &Message{
		Func: function,
		Args: args,
	}

	go func() {
		if err := e.send(msg); err != nil {
			log.Printf("Notify Error: %v", err)
		}
	}()
}

// NotifyRaw calls notify with precompiled args
func (e *Endpoint) NotifyRaw(bytes []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.conn.WriteMessage(websocket.TextMessage, bytes)
}
