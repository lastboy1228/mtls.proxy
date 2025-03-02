package gost

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-log/log"
)

type httpConnector struct {
	User *url.Userinfo
}

// HTTPConnector creates a Connector for HTTP proxy client.
// It accepts an optional auth info for HTTP Basic Authentication.
func HTTPConnector(user *url.Userinfo) Connector {
	return &httpConnector{User: user}
}

func (c *httpConnector) Connect(conn net.Conn, address string, options ...ConnectOption) (net.Conn, error) {
	return c.ConnectContext(context.Background(), conn, "tcp", address, options...)
}

func (c *httpConnector) ConnectContext(ctx context.Context, conn net.Conn, network, address string, options ...ConnectOption) (net.Conn, error) {
	switch network {
	case "udp", "udp4", "udp6":
		return nil, fmt.Errorf("%s unsupported", network)
	}

	opts := &ConnectOptions{}
	for _, option := range options {
		option(opts)
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = ConnectTimeout
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}

	conn.SetDeadline(time.Now().Add(timeout))
	defer conn.SetDeadline(time.Time{})

	req := &http.Request{
		Method:     http.MethodConnect,
		URL:        &url.URL{Host: address},
		Host:       address,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Proxy-Connection", "keep-alive")

	user := opts.User
	if user == nil {
		user = c.User
	}

	if user != nil {
		u := user.Username()
		p, _ := user.Password()
		req.Header.Set("Proxy-Authorization",
			"Basic "+base64.StdEncoding.EncodeToString([]byte(u+":"+p)))
	}

	if err := req.Write(conn); err != nil {
		return nil, err
	}

	if Debug {
		dump, _ := httputil.DumpRequest(req, false)
		log.Log(string(dump))
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return nil, err
	}

	if Debug {
		dump, _ := httputil.DumpResponse(resp, false)
		log.Log(string(dump))
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", resp.Status)
	}

	return conn, nil
}

type httpHandler struct {
	options *HandlerOptions
}

// HTTPHandler creates a server Handler for HTTP proxy server.
func HTTPHandler(opts ...HandlerOption) Handler {
	h := &httpHandler{}
	h.Init(opts...)
	return h
}

func (h *httpHandler) Init(options ...HandlerOption) {
	if h.options == nil {
		h.options = &HandlerOptions{}
	}
	for _, opt := range options {
		opt(h.options)
	}
}

func (h *httpHandler) Handle(conn net.Conn) {
	defer conn.Close()

	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		log.Logf("[http] %s - %s : %s", conn.RemoteAddr(), conn.LocalAddr(), err)
		return
	}
	defer req.Body.Close()

	h.handleRequest(conn, req)
}

func (h *httpHandler) handleRequest(conn net.Conn, req *http.Request) {
	if req == nil {
		return
	}

	// try to get the actual host.
	if v := req.Header.Get("Gost-Target"); v != "" {
		if h, err := decodeServerName(v); err == nil {
			req.Host = h
		}
	}

	host := req.Host
	if _, port, _ := net.SplitHostPort(host); port == "" {
		host = net.JoinHostPort(host, "80")
	}

	u, _, _ := basicProxyAuth(req.Header.Get("Proxy-Authorization"))
	if u != "" {
		u += "@"
	}

	if Debug {
		dump, _ := httputil.DumpRequest(req, false)
		log.Logf("[http] %s -> %s\n%s", conn.RemoteAddr(), conn.LocalAddr(), string(dump))
	}

	req.Header.Del("Gost-Target")

	resp := &http.Response{
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{},
	}

	proxyAgent := DefaultProxyAgent
	if h.options.ProxyAgent != "" {
		proxyAgent = h.options.ProxyAgent
	}
	resp.Header.Add("Proxy-Agent", proxyAgent)

	if !Can("tcp", host, h.options.Whitelist, h.options.Blacklist) {
		log.Logf("[http] %s - %s : Unauthorized to tcp connect to %s",
			conn.RemoteAddr(), conn.LocalAddr(), host)
		resp.StatusCode = http.StatusForbidden

		if Debug {
			dump, _ := httputil.DumpResponse(resp, false)
			log.Logf("[http] %s <- %s\n%s", conn.RemoteAddr(), conn.LocalAddr(), string(dump))
		}

		resp.Write(conn)
		return
	}

	if h.options.Bypass.Contains(host) {
		resp.StatusCode = http.StatusForbidden

		log.Logf("[http] %s - %s bypass %s",
			conn.RemoteAddr(), conn.LocalAddr(), host)
		if Debug {
			dump, _ := httputil.DumpResponse(resp, false)
			log.Logf("[http] %s <- %s\n%s", conn.RemoteAddr(), conn.LocalAddr(), string(dump))
		}

		resp.Write(conn)
		return
	}

	if !h.authenticate(conn, req, resp) {
		return
	}

	if req.Method == "PRI" || (req.Method != http.MethodConnect && req.URL.Scheme != "http") {
		resp.StatusCode = http.StatusBadRequest

		if Debug {
			dump, _ := httputil.DumpResponse(resp, false)
			log.Logf("[http] %s <- %s\n%s",
				conn.RemoteAddr(), conn.LocalAddr(), string(dump))
		}

		resp.Write(conn)
		return
	}

	req.Header.Del("Proxy-Authorization")

	retries := 1
	if h.options.Chain != nil && h.options.Chain.Retries > 0 {
		retries = h.options.Chain.Retries
	}
	if h.options.Retries > 0 {
		retries = h.options.Retries
	}

	var err error
	var cc net.Conn
	var routeChain *Chain
	for i := 0; i < retries; i++ {
		routeChain, err = h.options.Chain.selectRouteFor(host)
		if err != nil {
			log.Logf("[http] %s -> %s : %s",
				conn.RemoteAddr(), conn.LocalAddr(), err)
			continue
		}

		buf := bytes.Buffer{}
		// listen handler（-L）的地址：h.options.Node.String()
		// 客户端的地址
		fmt.Fprintf(&buf, "%s --> ", conn.RemoteAddr())
		for _, nd := range routeChain.route {
			// forwarder的地址
			fmt.Fprintf(&buf, "%d@%s -> ", nd.ID, nd.String())
		}
		fmt.Fprintf(&buf, "(target)%s", host)

		// forward http request
		lastNode := routeChain.LastNode()
		if req.Method != http.MethodConnect &&
			lastNode.Protocol == "http" &&
			!h.options.HTTPTunnel {
			err = h.forwardRequest(conn, req, routeChain)
			if err == nil {
				return
			}
			log.Logf("[http] %s -> %s : %s", conn.RemoteAddr(), conn.LocalAddr(), err)
			continue
		}

		cc, err = routeChain.Dial(host,
			TimeoutChainOption(h.options.Timeout),
			HostsChainOption(h.options.Hosts),
			ResolverChainOption(h.options.Resolver),
		)
		if err == nil {
			log.Log("[route]", buf.String())
			break
		}
		fmt.Fprintf(&buf, ": %s", err)
		log.Log("[route]", buf.String())
	}

	if err != nil {
		resp.StatusCode = http.StatusServiceUnavailable

		if Debug {
			dump, _ := httputil.DumpResponse(resp, false)
			log.Logf("[http] %s <- %s\n%s", conn.RemoteAddr(), conn.LocalAddr(), string(dump))
		}

		resp.Write(conn)
		return
	}
	defer func() {
		switch cc.(type) {
		case *quicConn:
			log.Logf("quic stream for (client)%s <> %s closing", conn.RemoteAddr(), req.Host)
		case *http2Conn:
			log.Logf("http2 stream for (client)%s <> %s closing", conn.RemoteAddr(), req.Host)
		}
		cc.Close()
	}()

	if req.Method == http.MethodConnect {
		b := []byte("HTTP/1.1 200 Connection established\r\n" +
			"Proxy-Agent: " + proxyAgent + "\r\n\r\n")
		if Debug {
			log.Logf("[http] %s <- %s\n%s", conn.RemoteAddr(), conn.LocalAddr(), string(b))
		}
		conn.Write(b)
	} else {
		req.Header.Del("Proxy-Connection")

		if err = req.Write(cc); err != nil {
			log.Logf("[http] %s -> %s : %s", conn.RemoteAddr(), conn.LocalAddr(), err)
			return
		}
	}
	transport(conn, cc)
}

func (h *httpHandler) authenticate(conn net.Conn, req *http.Request, resp *http.Response) (ok bool) {
	u, p, _ := basicProxyAuth(req.Header.Get("Proxy-Authorization"))
	if Debug && (u != "" || p != "") {
		log.Logf("[http] %s -> %s : Authorization '%s' '%s'",
			conn.RemoteAddr(), conn.LocalAddr(), u, p)
	}
	if h.options.Authenticator == nil || h.options.Authenticator.Authenticate(u, p) {
		return true
	}

	// probing resistance is enabled, and knocking host is mismatch.
	if ss := strings.SplitN(h.options.ProbeResist, ":", 2); len(ss) == 2 &&
		(h.options.KnockingHost == "" || !strings.EqualFold(req.URL.Hostname(), h.options.KnockingHost)) {
		resp.StatusCode = http.StatusServiceUnavailable // default status code

		switch ss[0] {
		case "code":
			resp.StatusCode, _ = strconv.Atoi(ss[1])
		case "web":
			url := ss[1]
			if !strings.HasPrefix(url, "http") {
				url = "http://" + url
			}
			if r, err := http.Get(url); err == nil {
				resp = r
			}
		case "host":
			cc, err := net.Dial("tcp", ss[1])
			if err == nil {
				defer cc.Close()

				req.Write(cc)
				log.Logf("[http] %s <-> %s : forward to %s",
					conn.RemoteAddr(), conn.LocalAddr(), ss[1])
				transport(conn, cc)
				log.Logf("[http] %s >-< %s : forward to %s",
					conn.RemoteAddr(), conn.LocalAddr(), ss[1])
				return
			}
		case "file":
			f, _ := os.Open(ss[1])
			if f != nil {
				resp.StatusCode = http.StatusOK
				if finfo, _ := f.Stat(); finfo != nil {
					resp.ContentLength = finfo.Size()
				}
				resp.Header.Set("Content-Type", "text/html")
				resp.Body = f
			}
		}
	}

	if resp.StatusCode == 0 {
		log.Logf("[http] %s <- %s : proxy authentication required",
			conn.RemoteAddr(), conn.LocalAddr())
		resp.StatusCode = http.StatusProxyAuthRequired
		resp.Header.Add("Proxy-Authenticate", "Basic realm=\"gost\"")
		if strings.ToLower(req.Header.Get("Proxy-Connection")) == "keep-alive" {
			// XXX libcurl will keep sending auth request in same conn
			// which we don't supported yet.
			resp.Header.Add("Connection", "close")
			resp.Header.Add("Proxy-Connection", "close")
		}
	} else {
		resp.Header = http.Header{}
		resp.Header.Set("Server", "nginx/1.14.1")
		resp.Header.Set("Date", time.Now().Format(http.TimeFormat))
		if resp.StatusCode == http.StatusOK {
			resp.Header.Set("Connection", "keep-alive")
		}
	}

	if Debug {
		dump, _ := httputil.DumpResponse(resp, false)
		log.Logf("[http] %s <- %s\n%s",
			conn.RemoteAddr(), conn.LocalAddr(), string(dump))
	}

	resp.Write(conn)
	return
}

func (h *httpHandler) forwardRequest(conn net.Conn, req *http.Request, route *Chain) error {
	if route.IsEmpty() {
		return nil
	}

	host := req.Host
	var userpass string

	if user := route.LastNode().User; user != nil {
		u := user.Username()
		p, _ := user.Password()
		userpass = base64.StdEncoding.EncodeToString([]byte(u + ":" + p))
	}

	cc, err := route.Conn()
	if err != nil {
		return err
	}
	defer cc.Close()

	errc := make(chan error, 1)
	go func() {
		errc <- copyBuffer(conn, cc)
	}()

	go func() {
		for {
			if userpass != "" {
				req.Header.Set("Proxy-Authorization", "Basic "+userpass)
			}

			cc.SetWriteDeadline(time.Now().Add(WriteTimeout))
			if !req.URL.IsAbs() {
				req.URL.Scheme = "http" // make sure that the URL is absolute
			}
			err := req.WriteProxy(cc)
			if err != nil {
				log.Logf("[http] %s -> %s : %s", conn.RemoteAddr(), conn.LocalAddr(), err)
				errc <- err
				return
			}
			cc.SetWriteDeadline(time.Time{})

			req, err = http.ReadRequest(bufio.NewReader(conn))
			if err != nil {
				errc <- err
				return
			}

			if Debug {
				dump, _ := httputil.DumpRequest(req, false)
				log.Logf("[http] %s -> %s\n%s",
					conn.RemoteAddr(), conn.LocalAddr(), string(dump))
			}
		}
	}()

	log.Logf("[http] %s <-> %s", conn.RemoteAddr(), host)
	<-errc
	log.Logf("[http] %s >-< %s", conn.RemoteAddr(), host)

	return nil
}

func basicProxyAuth(proxyAuth string) (username, password string, ok bool) {
	if proxyAuth == "" {
		return
	}

	if !strings.HasPrefix(proxyAuth, "Basic ") {
		return
	}
	c, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(proxyAuth, "Basic "))
	if err != nil {
		return
	}
	cs := string(c)
	s := strings.IndexByte(cs, ':')
	if s < 0 {
		return
	}

	return cs[:s], cs[s+1:], true
}
