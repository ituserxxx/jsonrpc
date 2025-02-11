package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/token"
	"log"
	"net/http"
	"reflect"
	"sync"
)

var (
	typeOfError   = reflect.TypeOf((*error)(nil)).Elem()
	typeOfContext = reflect.TypeOf((*context.Context)(nil)).Elem()
)
var (
	errServerInvalidParams = errors.New("invalid request params type format")
	errServerInvalidReturn = errors.New("invalid return type format")
)

// Server represents a JSON-RPC server.
type Server struct {
	handler sync.Map
	// cors map
	Cors map[string]string
}

type handlerType struct {
	f       reflect.Value
	ptype   reflect.Type
	rtype   reflect.Type
	numArgs int
}

// NewServer returns a new Server.
func NewServer() *Server {
	return &Server{}
}

// HandleFunc registers the handle function for the given JSON-RPC method.
func (s *Server) HandleFunc(method string, handler interface{}) error {
	h := reflect.ValueOf(handler)
	numArgs, ptype, rtype, err := inspectHandler(h)
	if err != nil {
		return fmt.Errorf("jsonrpc: %v", err)
	}
	s.handler.Store(method, handlerType{f: h, ptype: ptype, rtype: rtype, numArgs: numArgs})
	return nil
}

func inspectHandler(h reflect.Value) (numArgs int, ptype, rtype reflect.Type, err error) {
	ht := h.Type()
	if hkind := h.Kind(); hkind != reflect.Func {
		err = fmt.Errorf("invalid handler type: expected func, got %v", hkind)
		return
	}

	numArgs = ht.NumIn()
	if numArgs != 2 && numArgs != 1 {
		err = fmt.Errorf("invalid number of args: expected %v, got %v", 2, ht.NumIn())
		return
	}

	if ctxType := ht.In(0); ctxType != typeOfContext {
		err = fmt.Errorf("invalid first arg type: expected context.Context, got %v", ctxType)
		return
	}

	if numArgs == 2 {
		ptype = ht.In(1)
		if !isExportedOrBuiltinType(ptype) {
			err = fmt.Errorf("invalid second arg type: expected exported or builtin")
			return
		}
	}

	if numOut := ht.NumOut(); numOut != 2 {
		err = fmt.Errorf("invalid number of returns: expected 2, got %v", numOut)
		return
	}

	rtype = ht.Out(0)
	if !isExportedOrBuiltinType(rtype) {
		err = fmt.Errorf("invalid first return type: expected exported or builtin")
		return
	}

	if errorType := ht.Out(1); errorType != typeOfError {
		err = fmt.Errorf("invalid second return type: expected error, got %v", errorType)
		return
	}
	return
}

// ServeHTTP responds to an JSON-RPC request and executes the requested method.
func (s *Server) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if len(s.Cors) >0{
		for k1, v1 := range s.Cors {
			rw.Header().Add(k1,v1)
		}
	}
	// Only POST methods are jsonrpc valid calls
	if r.Method != "POST" {
		rw.WriteHeader(http.StatusNotFound)
		rw.Write([]byte("Not found"))
		return
	}

	ctx := r.Context()
	req, err := decodeRequestFromReader(r.Body)
	defer r.Body.Close()
	if errors.Is(err, errInvalidEncodedJSON) {
		sendResponse(rw, errResponse(null, ErrorParseError))
		return
	}
	if errors.Is(err, errInvalidDecodedMessage) {
		sendResponse(rw, errResponse(req.ID, ErrInvalidRequest))
		return
	}

	method, ok := s.handler.Load(req.Method)
	if !ok {
		sendResponse(rw, errResponse(req.ID, ErrMethodNotFound))
		return
	}

	htype, _ := method.(handlerType)
	if req.isNotification {
		_, err := callMethod(ctx, req, htype)
		if errors.Is(err, errServerInvalidParams) {
			log.Print("jsonrpc: notification: ", err)
			return
		}
		rw.WriteHeader(http.StatusOK)
		rw.Write([]byte(""))
		return
	}

	ret, err := callMethod(ctx, req, htype)
	if errors.Is(err, errServerInvalidParams) {
		sendResponse(rw, errResponse(req.ID, ErrInvalidParams))
		return
	}

	result, err := encodeMethodReturn(ret)
	if errors.Is(err, errServerInvalidReturn) {
		sendResponse(rw, errResponse(req.ID, ErrInternalError))
		return
	}
	if err, ok := err.(*Error); ok {
		sendResponse(rw, errResponse(req.ID, err))
		return
	}

	sendResponse(rw, &Response{
		id:     req.ID,
		error:  nil,
		result: (json.RawMessage)(result),
	})
}

func sendResponse(rw http.ResponseWriter, resp *Response) {
	b, err := resp.bytes()
	if err != nil {
		log.Printf("jsonrpc: sending response: %v", err)
		return
	}
	_, err = rw.Write(b)
	if err != nil {
		log.Printf("jsonrpc: sending response: %v", err)
	}
}

func callMethod(ctx context.Context, req *request, htype handlerType) ([]reflect.Value, error) {
	var retv []reflect.Value
	if htype.numArgs == 1 {
		retv = htype.f.Call([]reflect.Value{reflect.ValueOf(ctx)})
		return retv, nil
	}

	var pvalue, pzero reflect.Value
	pIsValue := false
	if htype.ptype.Kind() == reflect.Ptr {
		pvalue = reflect.New(htype.ptype.Elem())
		pzero = reflect.New(htype.ptype.Elem())
	} else {
		pvalue = reflect.New(htype.ptype)
		pzero = reflect.New(htype.ptype)
		pIsValue = true
	}

	// here pvalue is guaranteed to be a ptr
	// QUESTION: if pvalue doesnt change params should be invalid?
	if req.Params == nil || string(req.Params) == string(null) {
		return nil, errServerInvalidParams
	}
	if err := json.Unmarshal(req.Params, pvalue.Interface()); err != nil || pvalue.Elem().Interface() == pzero.Elem().Interface() {
		return nil, errServerInvalidParams
	}

	if pIsValue {
		retv = htype.f.Call([]reflect.Value{reflect.ValueOf(ctx), pvalue.Elem()})
	} else {
		retv = htype.f.Call([]reflect.Value{reflect.ValueOf(ctx), pvalue})
	}
	return retv, nil
}

func encodeMethodReturn(ret []reflect.Value) (json.RawMessage, error) {
	outErr := ret[1].Interface()
	switch err := outErr.(type) {
	case *Error:
		return nil, err
	case error:
		return nil, &Error{Code: -32000, Message: err.Error()}
	default:
	}

	result, err := json.Marshal(ret[0].Interface())
	if err != nil {
		// this should not happen if the output is well defined
		return nil, errServerInvalidReturn
	}
	return result, nil
}

func isExportedOrBuiltinType(t reflect.Type) bool {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	// PkgPath will be non-empty even for an exported type,
	// so we need to check the type name as well.
	return token.IsExported(t.Name()) || t.PkgPath() == ""
}
