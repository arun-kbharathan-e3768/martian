// Copyright 2015 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package martian

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"internal/poll"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/google/martian/v3/mitm"
	"github.com/google/martian/v3/nosigpipe"
	"github.com/google/martian/v3/proxyutil"
	"github.com/google/martian/v3/trafficshape"
)

var errClose = errors.New("closing connection")
var noop = Noop("martian")

func isCloseable(err error) bool {
	if neterr, ok := err.(net.Error); ok && neterr.Timeout() {
		fmt.Printf("martian: Closable Nettime out: %v\n", err)
		return true
	}

	switch err {
	case io.EOF:
		fmt.Printf("martian: Closable EOF")
		return true
	case io.ErrClosedPipe:
		fmt.Printf("martian: Closable ErrClosedPipe")
		return true
	case errClose:
		fmt.Printf("martian: Closable errClose")
		return true
	}

	return false
}

// Proxy is an HTTP proxy with support for TLS MITM and customizable behavior.
type Proxy struct {
	roundTripper http.RoundTripper
	dial         func(string, string) (net.Conn, error)
	timeout      time.Duration
	mitm         *mitm.Config
	proxyURL     *url.URL
	conns        sync.WaitGroup
	connsMu      sync.Mutex // protects conns.Add/Wait from concurrent access
	closing      chan bool

	reqmod RequestModifier
	resmod ResponseModifier
}

// NewProxy returns a new HTTP proxy.
func NewProxy() *Proxy {
	proxy := &Proxy{
		roundTripper: &http.Transport{
			// TODO(adamtanner): This forces the http.Transport to not upgrade requests
			// to HTTP/2 in Go 1.6+. Remove this once Martian can support HTTP/2.
			TLSNextProto:          make(map[string]func(string, *tls.Conn) http.RoundTripper),
			Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
		timeout: 5 * time.Minute,
		closing: make(chan bool),
		reqmod:  noop,
		resmod:  noop,
	}
	proxy.SetDial((&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).Dial)
	return proxy
}

// GetRoundTripper gets the http.RoundTripper of the proxy.
func (p *Proxy) GetRoundTripper() http.RoundTripper {
	return p.roundTripper
}

// SetRoundTripper sets the http.RoundTripper of the proxy.
func (p *Proxy) SetRoundTripper(rt http.RoundTripper) {
	p.roundTripper = rt

	if tr, ok := p.roundTripper.(*http.Transport); ok {
		tr.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
		tr.Proxy = http.ProxyURL(p.proxyURL)
		tr.Dial = p.dial
	}
}

// SetDownstreamProxy sets the proxy that receives requests from the upstream
// proxy.
func (p *Proxy) SetDownstreamProxy(proxyURL *url.URL) {
	p.proxyURL = proxyURL

	if tr, ok := p.roundTripper.(*http.Transport); ok {
		tr.Proxy = http.ProxyURL(p.proxyURL)
	}
}

// SetTimeout sets the request timeout of the proxy.
func (p *Proxy) SetTimeout(timeout time.Duration) {
	p.timeout = timeout
}

// SetMITM sets the config to use for MITMing of CONNECT requests.
func (p *Proxy) SetMITM(config *mitm.Config) {
	p.mitm = config
}

// SetDial sets the dial func used to establish a connection.
func (p *Proxy) SetDial(dial func(string, string) (net.Conn, error)) {
	p.dial = func(a, b string) (net.Conn, error) {
		c, e := dial(a, b)
		nosigpipe.IgnoreSIGPIPE(c)
		return c, e
	}

	if tr, ok := p.roundTripper.(*http.Transport); ok {
		tr.Dial = p.dial
	}
}

// Close sets the proxy to the closing state so it stops receiving new connections,
// finishes processing any inflight requests, and closes existing connections without
// reading anymore requests from them.
func (p *Proxy) Close() {
	fmt.Printf("martian: closing down proxy")

	close(p.closing)

	fmt.Printf("martian: waiting for connections to close")
	p.connsMu.Lock()
	p.conns.Wait()
	p.connsMu.Unlock()
	fmt.Printf("martian: all connections closed")
}

// Closing returns whether the proxy is in the closing state.
func (p *Proxy) Closing() bool {
	select {
	case <-p.closing:
		return true
	default:
		return false
	}
}

// SetRequestModifier sets the request modifier.
func (p *Proxy) SetRequestModifier(reqmod RequestModifier) {
	if reqmod == nil {
		reqmod = noop
	}

	p.reqmod = reqmod
}

// SetResponseModifier sets the response modifier.
func (p *Proxy) SetResponseModifier(resmod ResponseModifier) {
	if resmod == nil {
		resmod = noop
	}

	p.resmod = resmod
}

// Serve accepts connections from the listener and handles the requests.
func (p *Proxy) Serve(l net.Listener) error {
	defer l.Close()

	var delay time.Duration
	for {
		if p.Closing() {
			return nil
		}

		conn, err := l.Accept()
		fmt.Println("received connection")
		nosigpipe.IgnoreSIGPIPE(conn)
		if err != nil {
			if nerr, ok := err.(net.Error); ok && nerr.Temporary() {
				if delay == 0 {
					delay = 5 * time.Millisecond
				} else {
					delay *= 2
				}
				if max := time.Second; delay > max {
					delay = max
				}

				fmt.Printf("martian: temporary error on accept: %v\n", err)
				time.Sleep(delay)
				continue
			}

			if errors.Is(err, net.ErrClosed) {
				fmt.Printf("martian: listener closed, returning")
				return err
			}

			fmt.Printf("martian: failed to accept: %v\n", err)
			return err
		}
		delay = 0
		fmt.Printf("martian: accepted connection from %s\n", conn.RemoteAddr())

		if tconn, ok := conn.(*net.TCPConn); ok {
			tconn.SetKeepAlive(true)
			tconn.SetKeepAlivePeriod(3 * time.Minute)
		}
		fmt.Println("handleloop")
		go p.handleLoop(conn)
	}
}

func (p *Proxy) handleLoop(conn net.Conn) {
	p.connsMu.Lock()
	p.conns.Add(1)
	p.connsMu.Unlock()
	defer p.conns.Done()
	defer conn.Close()
	if p.Closing() {
		return
	}

	brw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))

	s, err := newSession(conn, brw)
	if err != nil {
		fmt.Printf("martian: failed to create session: %v\n", err)
		return
	}

	ctx, err := withSession(s)
	if err != nil {
		fmt.Printf("martian: failed to create context: %v\n", err)
		return
	}

	for {
		deadline := time.Now().Add(p.timeout)
		conn.SetDeadline(deadline)
		fmt.Println("handling")
		err := p.handle(ctx, conn, brw)
		if isCloseable(err) {
			fmt.Printf("martian: closing connection: %v\n", conn.RemoteAddr())
			return
		}
	}
}

func (p *Proxy) readRequest(ctx *Context, conn net.Conn, brw *bufio.ReadWriter) (*http.Request, error) {
	var req *http.Request
	fmt.Println("Reading request")
	reqc := make(chan *http.Request, 1)
	errc := make(chan error, 1)
	go func() {
		tr := io.TeeReader(brw.Reader, os.Stdout)
		btr := bufio.NewReader(tr)
		r, err := http.ReadRequest(btr)
		if err != nil {
			errc <- err
			return
		}
		reqc <- r
	}()
	select {
	case err := <-errc:
		if isCloseable(err) {
			fmt.Printf("martian: connection closed prematurely: %v\n", err)
		} else {
			fmt.Printf("martian: failed to read request: %v\n", err)
		}

		// TODO: TCPConn.WriteClose() to avoid sending an RST to the client.

		return nil, errClose
	case req = <-reqc:
	case <-p.closing:
		return nil, errClose
	}

	return req, nil
}

func (p *Proxy) handleConnectRequest(ctx *Context, req *http.Request, session *Session, brw *bufio.ReadWriter, conn net.Conn) error {
	if err := p.reqmod.ModifyRequest(req); err != nil {
		fmt.Printf("martian: error modifying CONNECT request: %v\n", err)
		proxyutil.Warning(req.Header, err)
	}
	if session.Hijacked() {
		fmt.Printf("martian: connection hijacked by request modifier")
		return nil
	}

	if p.mitm != nil {
		fmt.Printf("martian: attempting MITM for connection: %s / %s\n", req.Host, req.URL.String())

		res := proxyutil.NewResponse(200, nil, req)

		if err := p.resmod.ModifyResponse(res); err != nil {
			fmt.Printf("martian: error modifying CONNECT response: %v\n", err)
			proxyutil.Warning(res.Header, err)
		}
		if session.Hijacked() {
			fmt.Printf("martian: connection hijacked by response modifier")
			return nil
		}

		w := io.MultiWriter(os.Stdout, brw)

		if err := res.Write(w); err != nil {
			fmt.Printf("martian: got error while writing response back to client: %v\n", err)
		}
		if err := brw.Flush(); err != nil {
			fmt.Printf("martian: got error while flushing response back to client: %v\n", err)
		}

		fmt.Printf("martian: completed MITM for connection: %s\n", req.Host)

		b := make([]byte, 1)
		if _, err := brw.Read(b); err != nil {
			fmt.Printf("martian: error peeking message through CONNECT tunnel to determine type: %v\n", err)
		}

		// Drain all of the rest of the buffered data.
		buf := make([]byte, brw.Reader.Buffered())
		brw.Read(buf)

		// 22 is the TLS handshake.
		// https://tools.ietf.org/html/rfc5246#section-6.2.1
		if b[0] == 22 {
			// Prepend the previously read data to be read again by
			// http.ReadRequest.
			tlsconn := tls.Server(&peekedConn{conn, io.MultiReader(bytes.NewReader(b), bytes.NewReader(buf), conn)}, p.mitm.TLSForHost(req.Host))

			if err := tlsconn.Handshake(); err != nil {
				p.mitm.HandshakeErrorCallback(req, err)
				return err
			}
			if tlsconn.ConnectionState().NegotiatedProtocol == "h2" {
				return p.mitm.H2Config().Proxy(p.closing, tlsconn, req.URL)
			}

			var nconn net.Conn
			nconn = tlsconn
			// If the original connection is a traffic shaped connection, wrap the tls
			// connection inside a traffic shaped connection too.
			if ptsconn, ok := conn.(*trafficshape.Conn); ok {
				nconn = ptsconn.Listener.GetTrafficShapedConn(tlsconn)
			}
			brw.Writer.Reset(nconn)
			brw.Reader.Reset(nconn)
			return p.handle(ctx, nconn, brw)
		}

		// Prepend the previously read data to be read again by http.ReadRequest.
		brw.Reader.Reset(io.MultiReader(bytes.NewReader(b), bytes.NewReader(buf), conn))
		return p.handle(ctx, conn, brw)
	}

	fmt.Printf("martian: attempting to establish CONNECT tunnel: %s\n", req.URL.Host)
	res, cconn, cerr := p.connect(req)
	if cerr != nil {
		fmt.Printf("martian: failed to CONNECT: %v\n", cerr)
		res = proxyutil.NewResponse(502, nil, req)
		proxyutil.Warning(res.Header, cerr)

		if err := p.resmod.ModifyResponse(res); err != nil {
			fmt.Printf("martian: error modifying CONNECT response: %v\n", err)
			proxyutil.Warning(res.Header, err)
		}
		if session.Hijacked() {
			fmt.Printf("martian: connection hijacked by response modifier")
			return nil
		}

		if err := res.Write(brw); err != nil {
			fmt.Printf("martian: got error while writing response back to client: %v\n", err)
		}
		err := brw.Flush()
		if err != nil {
			fmt.Printf("martian: got error while flushing response back to client: %v\n", err)
		}
		return err
	}
	defer res.Body.Close()
	defer cconn.Close()

	if err := p.resmod.ModifyResponse(res); err != nil {
		fmt.Printf("martian: error modifying CONNECT response: %v\n", err)
		proxyutil.Warning(res.Header, err)
	}
	if session.Hijacked() {
		fmt.Printf("martian: connection hijacked by response modifier")
		return nil
	}

	res.ContentLength = -1
	if err := res.Write(brw); err != nil {
		fmt.Printf("martian: got error while writing response back to client: %v\n", err)
	}
	if err := brw.Flush(); err != nil {
		fmt.Printf("martian: got error while flushing response back to client: %v\n", err)
	}

	cbw := bufio.NewWriter(cconn)
	cbr := bufio.NewReader(cconn)
	defer cbw.Flush()

	copySync := func(w io.Writer, r io.Reader, donec chan<- bool) {
		if _, err := io.Copy(w, r); err != nil && err != io.EOF {
			fmt.Printf("martian: failed to copy CONNECT tunnel: %v\n", err)
		}

		fmt.Printf("martian: CONNECT tunnel finished copying")
		donec <- true
	}

	donec := make(chan bool, 2)
	go copySync(cbw, brw, donec)
	go copySync(brw, cbr, donec)

	fmt.Printf("martian: established CONNECT tunnel, proxying traffic")
	<-donec
	<-donec
	fmt.Printf("martian: closed CONNECT tunnel")

	return errClose
}

func (p *Proxy) handle(ctx *Context, conn net.Conn, brw *bufio.ReadWriter) error {
	fmt.Printf("martian: waiting for request: %v\n", conn.RemoteAddr())

	req, err := p.readRequest(ctx, conn, brw)
	fmt.Println("read request")
	if err != nil {
		return err
	}
	defer req.Body.Close()

	session := ctx.Session()
	ctx, err = withSession(session)
	if err != nil {
		fmt.Printf("martian: failed to build new context: %v\n", err)
		return err
	}

	link(req, ctx)
	defer unlink(req)

	if tsconn, ok := conn.(*trafficshape.Conn); ok {
		wrconn := tsconn.GetWrappedConn()
		if sconn, ok := wrconn.(*tls.Conn); ok {
			session.MarkSecure()

			cs := sconn.ConnectionState()
			req.TLS = &cs
		}
	}

	if tconn, ok := conn.(*tls.Conn); ok {
		session.MarkSecure()

		cs := tconn.ConnectionState()
		req.TLS = &cs
	}

	req.URL.Scheme = "http"
	if session.IsSecure() {
		fmt.Printf("martian: forcing HTTPS inside secure session")
		req.URL.Scheme = "https"
	}

	req.RemoteAddr = conn.RemoteAddr().String()
	if req.URL.Host == "" {
		req.URL.Host = req.Host
	}

	if req.Method == "CONNECT" {
		return p.handleConnectRequest(ctx, req, session, brw, conn)
	}

	// Not a CONNECT request
	if err := p.reqmod.ModifyRequest(req); err != nil {
		fmt.Printf("martian: error modifying request: %v\n", err)
		proxyutil.Warning(req.Header, err)
	}
	if session.Hijacked() {
		return nil
	}
	fmt.Println("round tripping")
	// perform the HTTP roundtrip
	res, err := p.roundTrip(ctx, req)
	fmt.Println("round tripped")
	if err != nil {
		fmt.Printf("martian: failed to round trip: %v\n", err)
		res = proxyutil.NewResponse(502, nil, req)
		proxyutil.Warning(res.Header, err)
	}
	defer res.Body.Close()

	// set request to original request manually, res.Request may be changed in transport.
	// see https://github.com/google/martian/issues/298
	res.Request = req

	if err := p.resmod.ModifyResponse(res); err != nil {
		fmt.Printf("martian: error modifying response: %v\n", err)
		proxyutil.Warning(res.Header, err)
	}
	if session.Hijacked() {
		fmt.Printf("martian: connection hijacked by response modifier")
		return nil
	}

	var closing error
	if req.Close || res.Close || p.Closing() {
		fmt.Printf("martian: received close request: %v\n", req.RemoteAddr)
		res.Close = true
		closing = errClose
	}

	// check if conn is a traffic shaped connection.
	if ptsconn, ok := conn.(*trafficshape.Conn); ok {
		ptsconn.Context = &trafficshape.Context{}
		// Check if the request URL matches any URLRegex in Shapes. If so, set the connections's Context
		// with the required information, so that the Write() method of the Conn has access to it.
		for urlregex, buckets := range ptsconn.LocalBuckets {
			if match, _ := regexp.MatchString(urlregex, req.URL.String()); match {
				if rangeStart := proxyutil.GetRangeStart(res); rangeStart > -1 {
					dump, err := httputil.DumpResponse(res, false)
					if err != nil {
						return err
					}
					ptsconn.Context = &trafficshape.Context{
						Shaping:            true,
						Buckets:            buckets,
						GlobalBucket:       ptsconn.GlobalBuckets[urlregex],
						URLRegex:           urlregex,
						RangeStart:         rangeStart,
						ByteOffset:         rangeStart,
						HeaderLen:          int64(len(dump)),
						HeaderBytesWritten: 0,
					}
					// Get the next action to perform, if there.
					ptsconn.Context.NextActionInfo = ptsconn.GetNextActionFromByte(rangeStart)
					// Check if response lies in a throttled byte range.
					ptsconn.Context.ThrottleContext = ptsconn.GetCurrentThrottle(rangeStart)
					if ptsconn.Context.ThrottleContext.ThrottleNow {
						ptsconn.Context.Buckets.WriteBucket.SetCapacity(
							ptsconn.Context.ThrottleContext.Bandwidth)
					}
					fmt.Printf(
						"trafficshape: Request %s with Range Start: %d matches a Shaping request %s. Enforcing Traffic shaping.",
						req.URL, rangeStart, urlregex)
				}
				break
			}
		}
	}
	// var b bytes.Buffer
	w := io.MultiWriter(os.Stdout, brw)
	fmt.Println("write res")
	err = res.Write(w)
	fmt.Println("wrote res")
	if err != nil {
		fmt.Printf("martian: got error while writing response back to client: %v\n", err)
		if _, ok := err.(*trafficshape.ErrForceClose); ok {
			closing = errClose
		}
		if isOtherClosableError(err) {
			closing = errClose
		}
	}

	// nn, err := brw.Write(b.Bytes())
	// _ = nn
	// if err != nil {
	// 	fmt.Printf("martian: got error while writing response back to client: %v\n", err)
	// 	if _, ok := err.(*trafficshape.ErrForceClose); ok {
	// 		closing = errClose
	// 	}
	// }
	fmt.Println("flush")
	err = brw.Flush()
	fmt.Println("flushed")
	if err != nil {
		fmt.Printf("martian: got error while flushing response back to client: %v\n", err)
		if _, ok := err.(*trafficshape.ErrForceClose); ok {
			closing = errClose
		}
		if isOtherClosableError(err) {
			closing = errClose
		}
	}
	return closing
}

func isOtherClosableError(err error) bool {
	switch err {
	case syscall.EAGAIN, syscall.EINVAL, syscall.ENOENT:
		return true
	case io.ErrClosedPipe, io.ErrNoProgress, io.ErrShortBuffer, io.ErrShortWrite, io.ErrUnexpectedEOF:
		return true
	case fs.ErrClosed, fs.ErrExist, fs.ErrInvalid, fs.ErrNotExist, fs.ErrPermission:
		return true
	case os.ErrInvalid, os.ErrClosed:
		return true
	case poll.ErrFileClosing, poll.ErrNetClosing, poll.ErrDeadlineExceeded, poll.ErrNotPollable:
		return true
	case net.ErrClosed, net.ErrWriteToConnected:
		return true
	case http.ErrBodyReadAfterClose:
		return true
	}

	switch t := err.(type) {
	case *net.OpError:
		if t.Op == "dial" {
			println("Unknown host")
			return true
		} else if t.Op == "read" {
			println("Connection refused")
			return true
		}
	case syscall.Errno:
		if t == syscall.ECONNREFUSED {
			println("Connection refused")
			return true
		}
	case *os.PathError:
		if t == os.ErrClosed {
			return true
		}
	case net.Error:
		if t.Timeout() {
			return true
		}
	}
	return false
}

// A peekedConn subverts the net.Conn.Read implementation, primarily so that
// sniffed bytes can be transparently prepended.
type peekedConn struct {
	net.Conn
	r io.Reader
}

// Read allows control over the embedded net.Conn's read data. By using an
// io.MultiReader one can read from a conn, and then replace what they read, to
// be read again.
func (c *peekedConn) Read(buf []byte) (int, error) { return c.r.Read(buf) }

func (p *Proxy) roundTrip(ctx *Context, req *http.Request) (*http.Response, error) {
	if ctx.SkippingRoundTrip() {
		fmt.Printf("martian: skipping round trip")
		return proxyutil.NewResponse(200, nil, req), nil
	}

	return p.roundTripper.RoundTrip(req)
}

func (p *Proxy) connect(req *http.Request) (*http.Response, net.Conn, error) {
	if p.proxyURL != nil {
		fmt.Printf("martian: CONNECT with downstream proxy: %s\n", p.proxyURL.Host)

		conn, err := p.dial("tcp", p.proxyURL.Host)
		if err != nil {
			return nil, nil, err
		}
		pbw := bufio.NewWriter(conn)
		pbr := bufio.NewReader(conn)

		req.Write(pbw)
		pbw.Flush()

		res, err := http.ReadResponse(pbr, req)
		if err != nil {
			return nil, nil, err
		}

		return res, conn, nil
	}

	fmt.Printf("martian: CONNECT to host directly: %s\n", req.URL.Host)

	conn, err := p.dial("tcp", req.URL.Host)
	if err != nil {
		return nil, nil, err
	}

	return proxyutil.NewResponse(200, nil, req), conn, nil
}
