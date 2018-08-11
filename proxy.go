// Copyright 2018 ouqiang authors
//
// Licensed under the Apache License, Version 2.0 (the "License"): you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations
// under the License.

// Package goproxy HTTP(S)代理
package goproxy

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const (
	defaultTargetConnectTimeout   = 5 * time.Second
	defaultTargetReadWriteTimeout = 1 * time.Minute
	defaultClientReadWriteTimeout = 1 * time.Minute
)

var tunnelEstablishedResponseLine = []byte("HTTP/1.1 200 Connection established\r\n\r\n")

func makeTunnelRequestLine(addr string) string {
	return fmt.Sprintf("CONNECT %s HTTP/1.1\r\n\r\n", addr)
}

type options struct {
	disableKeepAlive bool
	delegate         Delegate
	transport        *http.Transport
}

type Option func(*options)

func WithDisableKeepAlive(disableKeepAlive bool) Option {
	return func(opt *options) {
		opt.disableKeepAlive = disableKeepAlive
	}
}

func WithDelegate(delegate Delegate) Option {
	return func(opt *options) {
		opt.delegate = delegate
	}
}

func WithTransport(t *http.Transport) Option {
	return func(opt *options) {
		opt.transport = t
	}
}

// New 创建proxy实例
func New(opt ...Option) *Proxy {
	opts := &options{}
	for _, o := range opt {
		o(opts)
	}
	if opts.delegate == nil {
		opts.delegate = &DefaultDelegate{}
	}
	if opts.transport == nil {
		opts.transport = &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
				DualStack: true,
			}).DialContext,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}

	p := &Proxy{}
	p.delegate = opts.delegate
	p.transport = opts.transport
	p.transport.DisableKeepAlives = opts.disableKeepAlive
	p.transport.Proxy = p.delegate.ParentProxy

	return p
}

// Proxy 实现了http.Handler接口
type Proxy struct {
	delegate      Delegate
	clientConnNum int32
	transport     *http.Transport
}

var _ http.Handler = &Proxy{}

// ServeHTTP 实现了http.Handler接口
func (p *Proxy) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	atomic.AddInt32(&p.clientConnNum, 1)
	defer func() {
		atomic.AddInt32(&p.clientConnNum, -1)
	}()
	ctx := &Context{
		Req:  req,
		Data: make(map[interface{}]interface{}),
	}
	defer p.delegate.Finish(ctx)
	p.delegate.Connect(ctx, rw)
	if ctx.abort {
		return
	}
	p.delegate.Auth(ctx, rw)
	if ctx.abort {
		return
	}

	switch req.Method {
	case http.MethodConnect:
		p.forwardTunnel(ctx, rw)
	default:
		p.forwardHTTP(ctx, rw)
	}
}

// ClientConnNum 获取客户端连接数
func (p *Proxy) ClientConnNum() int32 {
	return atomic.LoadInt32(&p.clientConnNum)
}

// HTTP转发
func (p *Proxy) forwardHTTP(ctx *Context, rw http.ResponseWriter) {
	p.delegate.BeforeRequest(ctx)
	if ctx.abort {
		return
	}
	removeIssueHeader(ctx.Req.Header)
	resp, err := p.transport.RoundTrip(ctx.Req)
	p.delegate.BeforeResponse(ctx, resp, err)
	if ctx.abort {
		return
	}
	if err != nil {
		p.delegate.ErrorLog(fmt.Errorf("HTTP请求错误: [URL: %s], 错误: %s", ctx.Req.URL, err))
		rw.WriteHeader(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	removeIssueHeader(resp.Header)
	copyHeader(rw.Header(), resp.Header)
	rw.WriteHeader(resp.StatusCode)
	io.Copy(rw, resp.Body)
}

// 隧道转发
func (p *Proxy) forwardTunnel(ctx *Context, rw http.ResponseWriter) {
	p.delegate.BeforeTunnelForward(ctx, rw)
	if ctx.abort {
		return
	}

	clientConn, err := p.hijacker(rw)
	if err != nil {
		p.delegate.ErrorLog(err)
		rw.WriteHeader(http.StatusBadGateway)
		return
	}
	defer clientConn.Close()
	parentProxyURL, err := p.delegate.ParentProxy(ctx.Req)
	if err != nil {
		p.delegate.ErrorLog(fmt.Errorf("解析代理地址错误: [%s] %s", ctx.Req.URL.Host, err))
		rw.WriteHeader(http.StatusBadGateway)
		return
	}
	targetAddr := ctx.Req.URL.Host
	if parentProxyURL != nil {
		targetAddr = parentProxyURL.Host
	}

	targetConn, err := net.DialTimeout("tcp", targetAddr, defaultTargetConnectTimeout)
	if err != nil {
		p.delegate.ErrorLog(fmt.Errorf("隧道转发连接目标服务器失败: [%s] [%s]", ctx.Req.URL.Host, err))
		rw.WriteHeader(http.StatusBadGateway)
		return
	}
	defer targetConn.Close()
	clientConn.SetDeadline(time.Now().Add(defaultClientReadWriteTimeout))
	targetConn.SetDeadline(time.Now().Add(defaultTargetReadWriteTimeout))
	if parentProxyURL == nil {
		_, err = clientConn.Write(tunnelEstablishedResponseLine)
		if err != nil {
			p.delegate.ErrorLog(fmt.Errorf("隧道连接成功,通知客户端错误: %s", err))
			return
		}
	} else {
		tunnelRequestLine := makeTunnelRequestLine(ctx.Req.URL.Host)
		targetConn.Write([]byte(tunnelRequestLine))
	}

	p.forwardTCP(clientConn, targetConn)
}

// TCP转发
func (p *Proxy) forwardTCP(src net.Conn, dst net.Conn) {
	go func() {
		io.Copy(src, dst)
		src.Close()
		dst.Close()
	}()

	io.Copy(dst, src)
	dst.Close()
	src.Close()
}

// 获取底层连接
func (p *Proxy) hijacker(rw http.ResponseWriter) (net.Conn, error) {
	hijacker, ok := rw.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("web server不支持Hijacker")
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		return nil, fmt.Errorf("hijacker错误: %s", err)
	}

	return conn, nil
}

// copyHeader 浅拷贝Header
func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

var hopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func removeIssueHeader(header http.Header) {
	removeConnectionHeaders(header)
	removeHopHopHeaders(header)
}

func removeConnectionHeaders(h http.Header) {
	if c := h.Get("Connection"); c != "" {
		for _, f := range strings.Split(c, ",") {
			if f = strings.TrimSpace(f); f != "" {
				h.Del(f)
			}
		}
	}
}

func removeHopHopHeaders(h http.Header) {
	for _, item := range hopHeaders {
		if h.Get(item) != "" {
			h.Del(item)
		}
	}
}
