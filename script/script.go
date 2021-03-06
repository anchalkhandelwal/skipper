// Package script provides lua scripting for skipper
package script

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"

	lua "github.com/yuin/gopher-lua"
	lua_parse "github.com/yuin/gopher-lua/parse"
	"github.com/zalando/skipper/filters"
	"github.com/zalando/skipper/script/base64"

	"github.com/cjoudrey/gluahttp"
	"github.com/cjoudrey/gluaurl"
	gjson "layeh.com/gopher-json"
)

// InitialPoolSize is the number of lua states created initially per route
var InitialPoolSize int = 3

// MaxPoolSize is the number of lua states stored per route - there may be more parallel
// requests, but only this number is cached.
var MaxPoolSize int = 10

type luaScript struct{}

// NewLuaScript creates a new filter spec for skipper
func NewLuaScript() filters.Spec {
	return &luaScript{}
}

// Name returns the name of the filter ("lua")
func (ls *luaScript) Name() string {
	return "lua"
}

// CreateFilter creates the filter
func (ls *luaScript) CreateFilter(config []interface{}) (filters.Filter, error) {
	if len(config) == 0 {
		return nil, filters.ErrInvalidFilterParameters
	}
	src, ok := config[0].(string)
	if !ok {
		return nil, filters.ErrInvalidFilterParameters
	}
	var params []string
	for _, p := range config[1:] {
		ps, ok := p.(string)
		if !ok {
			return nil, filters.ErrInvalidFilterParameters
		}
		params = append(params, ps)
	}

	s := &script{source: src, routeParams: params}
	if err := s.initScript(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *script) getState() (*lua.LState, error) {
	select {
	case L := <-s.pool:
		if L == nil {
			return nil, errors.New("pool closed")
		}
		return L, nil
	default:
		return s.newState()
	}
}

func (s *script) putState(L *lua.LState) {
	if s.pool == nil { // pool closed
		L.Close()
		return
	}
	select {
	case s.pool <- L:
	default: // pool full, close state
		L.Close()
	}
}

func (s *script) newState() (*lua.LState, error) {
	L := lua.NewState()
	L.PreloadModule("base64", base64.Loader)
	L.PreloadModule("http", gluahttp.NewHttpModule(&http.Client{}).Loader)
	L.PreloadModule("url", gluaurl.Loader)
	L.PreloadModule("json", gjson.Loader)

	L.Push(L.NewFunctionFromProto(s.proto))

	err := L.PCall(0, lua.MultRet, nil)
	if err != nil {
		log.Printf("ERROR loading `%s`: %s", s.source, err)
		L.Close()
		return nil, err
	}
	return L, nil
}

func (s *script) initScript() error {
	// Compile
	var reader io.Reader
	var name string

	if strings.HasSuffix(s.source, ".lua") {
		file, err := os.Open(s.source)
		defer file.Close()
		if err != nil {
			return err
		}
		reader = bufio.NewReader(file)
		name = s.source
	} else {
		reader = strings.NewReader(s.source)
		name = "<script>"
	}
	chunk, err := lua_parse.Parse(reader, name)
	if err != nil {
		return err
	}
	proto, err := lua.Compile(chunk, name)
	if err != nil {
		return err
	}
	s.proto = proto

	// Detect request and response functions
	L, err := s.newState()
	if err != nil {
		return err
	}
	defer L.Close()

	if fn := L.GetGlobal("request"); fn.Type() == lua.LTFunction {
		s.hasRequest = true
	}
	if fn := L.GetGlobal("response"); fn.Type() == lua.LTFunction {
		s.hasResponse = true
	}
	if !s.hasRequest && !s.hasResponse {
		return errors.New("at least one of `request` and `response` function must be present")
	}

	// Init state pool
	s.pool = make(chan *lua.LState, MaxPoolSize) // FIXME make configurable
	for i := 0; i < InitialPoolSize; i++ {
		L, err := s.newState()
		if err != nil {
			return err
		}
		s.putState(L)
	}
	return nil
}

type script struct {
	source                  string
	routeParams             []string
	pool                    chan *lua.LState
	proto                   *lua.FunctionProto
	hasRequest, hasResponse bool
}

func (s *script) Request(f filters.FilterContext) {
	if s.hasRequest {
		s.runFunc("request", f)
	}
}

func (s *script) Response(f filters.FilterContext) {
	if s.hasResponse {
		s.runFunc("response", f)
	}
}

func (s *script) runFunc(name string, f filters.FilterContext) {
	L, err := s.getState()
	if err != nil {
		log.Printf("ERROR: %s", err)
		return
	}
	defer s.putState(L)

	pt := L.CreateTable(0, 0)
	for _, p := range s.routeParams {
		parts := strings.SplitN(p, "=", 2)
		if len(parts) == 1 {
			parts = append(parts, "")
		}
		pt.RawSetString(parts[0], lua.LString(parts[1]))
	}

	err = L.CallByParam(
		lua.P{
			Fn:      L.GetGlobal(name),
			NRet:    0,
			Protect: true,
		},
		s.filterContextAsLuaTable(L, f),
		pt,
	)
	if err != nil {
		fmt.Printf("Error calling %s from %s: %s", name, s.source, err)
	}
}

func (s *script) filterContextAsLuaTable(L *lua.LState, f filters.FilterContext) *lua.LTable {
	// this will be passed as parameter to the lua functions
	// add metatable to dynamically access fields in the context
	t := L.CreateTable(0, 0)
	mt := L.CreateTable(0, 1)
	mt.RawSetString("__index", L.NewFunction(getContextValue(f)))
	L.SetMetatable(t, mt)
	return t
}

func serveRequest(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		t := s.Get(-1)
		r, ok := t.(*lua.LTable)
		if !ok {
			s.Push(lua.LString("invalid type, need a table"))
			return 1
		}
		res := &http.Response{}
		r.ForEach(serveTableWalk(res))
		f.Serve(res)
		return 0
	}
}

func serveTableWalk(res *http.Response) func(lua.LValue, lua.LValue) {
	return func(k, v lua.LValue) {
		s, ok := k.(lua.LString)
		if !ok {
			return
		}
		switch string(s) {
		case "status_code":
			n, ok := v.(lua.LNumber)
			if !ok {
				return
			}
			res.StatusCode = int(n)

		case "header":
			t, ok := v.(*lua.LTable)
			if !ok {
				return
			}
			h := make(http.Header)
			t.ForEach(serveHeaderWalk(h))
			res.Header = h

		case "body":
			var body []byte
			var err error
			switch v.Type() {
			case lua.LTString:
				data := string(v.(lua.LString))
				body = []byte(data)
			case lua.LTTable:
				body, err = gjson.Encode(v.(*lua.LTable))
				if err != nil {
					return
				}
			}
			res.Body = ioutil.NopCloser(bytes.NewBuffer(body))
		}
	}
}

func serveHeaderWalk(h http.Header) func(lua.LValue, lua.LValue) {
	return func(k, v lua.LValue) {
		h.Set(k.String(), v.String())
	}
}

func getContextValue(f filters.FilterContext) func(*lua.LState) int {
	var request, response, state_bag *lua.LTable
	var serve *lua.LFunction
	return func(s *lua.LState) int {
		key := s.ToString(-1)
		var ret lua.LValue
		switch key {
		case "request":
			// initialize access to request on first use
			if request == nil {
				request = s.CreateTable(0, 0)
				mt := s.CreateTable(0, 2)
				mt.RawSetString("__index", s.NewFunction(getRequestValue(f)))
				mt.RawSetString("__newindex", s.NewFunction(setRequestValue(f)))
				s.SetMetatable(request, mt)
			}
			ret = request
		case "response":
			if response == nil {
				response = s.CreateTable(0, 0)
				mt := s.CreateTable(0, 2)
				mt.RawSetString("__index", s.NewFunction(getResponseValue(f)))
				//mt.RawSetString("__newindex", s.NewFunction(setResponseValue(f)))
				s.SetMetatable(response, mt)
			}
			ret = response
		case "state_bag":
			if state_bag == nil {
				state_bag = s.CreateTable(0, 0)
				mt := s.CreateTable(0, 2)
				mt.RawSetString("__index", s.NewFunction(getStateBag(f)))
				mt.RawSetString("__newindex", s.NewFunction(setStateBag(f)))
				s.SetMetatable(state_bag, mt)
			}
			ret = state_bag
		case "serve":
			if serve == nil {
				serve = s.NewFunction(serveRequest(f))
			}
			ret = serve
		default:
			ret = lua.LNil
		}
		s.Push(ret)
		return 1
	}
}

func getRequestValue(f filters.FilterContext) func(*lua.LState) int {
	var header *lua.LTable
	return func(s *lua.LState) int {
		key := s.ToString(-1)
		var ret lua.LValue
		switch key {
		case "header":
			if header == nil {
				header = s.CreateTable(0, 0)
				mt := s.CreateTable(0, 2)
				mt.RawSetString("__index", s.NewFunction(getRequestHeader(f)))
				mt.RawSetString("__newindex", s.NewFunction(setRequestHeader(f)))
				s.SetMetatable(header, mt)
			}
			ret = header
		case "outgoing_host":
			ret = lua.LString(f.OutgoingHost())
		case "backend_url":
			ret = lua.LString(f.BackendUrl())
		case "host":
			ret = lua.LString(f.Request().Host)
		case "remote_addr":
			ret = lua.LString(f.Request().RemoteAddr)
		case "content_length":
			ret = lua.LNumber(f.Request().ContentLength)
		case "proto":
			ret = lua.LString(f.Request().Proto)
		case "method":
			ret = lua.LString(f.Request().Method)
		case "url":
			ret = lua.LString(f.Request().URL.String())
		default:
			ret = lua.LNil
		}
		s.Push(ret)
		return 1
	}
}

func setRequestValue(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		key := s.ToString(-2)
		switch key {
		case "outgoing_host":
			f.SetOutgoingHost(s.ToString(-1))
		case "url":
			u, err := url.Parse(s.ToString(-1))
			if err != nil {
				s.Push(lua.LString(err.Error()))
				return 1
			}
			f.Request().URL = u
		default:
			// do nothing for now
		}
		return 0
	}
}

func getResponseValue(f filters.FilterContext) func(*lua.LState) int {
	var header *lua.LTable
	return func(s *lua.LState) int {
		key := s.ToString(-1)
		var ret lua.LValue
		switch key {
		case "header":
			if header == nil {
				header = s.CreateTable(0, 0)
				mt := s.CreateTable(0, 2)
				mt.RawSetString("__index", s.NewFunction(getResponseHeader(f)))
				mt.RawSetString("__newindex", s.NewFunction(setResponseHeader(f)))
				s.SetMetatable(header, mt)
			}
			ret = header
		default:
			ret = lua.LNil
		}
		s.Push(ret)
		return 1
	}
}

func getStateBag(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		fld := s.ToString(-1)
		res, ok := f.StateBag()[fld]
		if !ok {
			s.Push(lua.LNil)
			return 1
		}
		switch res := res.(type) {
		case string:
			s.Push(lua.LString(res))
		case int:
			s.Push(lua.LNumber(res))
		case int64:
			s.Push(lua.LNumber(res))
		case float64:
			s.Push(lua.LNumber(res))
		default:
			s.Push(lua.LNil)
		}
		return 1
	}
}

func setStateBag(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		fld := s.ToString(-2)
		val := s.Get(-1)
		var res interface{}
		switch val.Type() {
		case lua.LTString:
			res = string(val.(lua.LString))
		case lua.LTNumber:
			res = float64(val.(lua.LNumber))
		default:
			s.Push(lua.LString("unsupported type for state bag"))
			return 1
		}

		f.StateBag()[fld] = res
		return 0
	}
}

func getRequestHeader(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		hdr := s.ToString(-1)
		res := f.Request().Header.Get(hdr)
		s.Push(lua.LString(res))
		return 1
	}
}

func setRequestHeader(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		lv := s.Get(-1)
		hdr := s.ToString(-2)
		switch lv.Type() {
		case lua.LTNil:
			f.Request().Header.Del(hdr)
		case lua.LTString:
			str := string(lv.(lua.LString))
			if str == "" {
				f.Request().Header.Del(hdr)
			} else {
				f.Request().Header.Set(hdr, str)
			}
		default:
			val := s.ToString(-1)
			f.Request().Header.Set(hdr, val)
		}
		return 0
	}
}

func getResponseHeader(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		hdr := s.ToString(-1)
		res := f.Response().Header.Get(hdr)
		s.Push(lua.LString(res))
		return 1
	}
}

func setResponseHeader(f filters.FilterContext) func(*lua.LState) int {
	return func(s *lua.LState) int {
		lv := s.Get(-1)
		hdr := s.ToString(-2)
		switch lv.Type() {
		case lua.LTNil:
			f.Response().Header.Del(hdr)
		case lua.LTString:
			str := string(lv.(lua.LString))
			if str == "" {
				f.Response().Header.Del(hdr)
			} else {
				f.Response().Header.Set(hdr, str)
			}
		default:
			val := s.ToString(-1)
			f.Response().Header.Set(hdr, val)
		}
		return 0
	}
}
