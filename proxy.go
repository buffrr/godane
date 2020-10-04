package letsdane

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"github.com/buffrr/letsdane/resolver"
	"github.com/elazarl/goproxy"
	"github.com/miekg/dns"
	"net"
	"net/http"
	"time"
)

type Config struct {
	Certificate *x509.Certificate
	PrivateKey  interface{}
	Validity    time.Duration
	Resolver    resolver.Resolver
	Verbose     bool
}

type tlsDialConfig struct {
	Fail error
	Host string
	Port string
	Network string
	IPs  []net.IP
	TLSA []*dns.TLSA
	Config *tls.Config
	Conn net.Conn
}

func tlsaFilterFunc(c *Config) goproxy.ReqConditionFunc {
	return func(req *http.Request, ctx *goproxy.ProxyCtx) bool {
		host, port, err := net.SplitHostPort(req.Host)
		if err != nil {
			ctx.Logf("proxy: invalid host %s", req.Host)
			return false
		}

		var blockError error
		var ips []net.IP

		ans, err := c.Resolver.LookupTLSA(port, "tcp", host, true)
		if err != nil {
			blockError = err
			ctx.Logf("proxy: tlsa lookup for host %s failed: %v", host, err)
		} else {
			ips, blockError = c.Resolver.LookupIP(host, true)
			if blockError != nil {
				ctx.Logf("proxy: ip lookup for host %s failed: %v", host, err)
			}
		}

		if blockError == nil {
			if len(ips) == 0 {
				ctx.Logf("proxy: no such host %s: skipping mitm", host)
				return false
			}

			if !tlsaSupported(ans) {
				ctx.Logf("proxy: host %s has no supported tlsa records skipping mitm", host)
				return false
			}
		}

		res := &tlsDialConfig{
			Fail: blockError,
			IPs:  ips,
			TLSA: ans,
			Host: host,
			Port: port,
			Network: "tcp",
		}
		ctx.UserData = res

		return true
	}
}

var handleConnect = func(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
	dialConfig, ok := ctx.UserData.(*tlsDialConfig)
	if !ok {
		return goproxy.RejectConnect, host
	}
	if dialConfig.Fail != nil {
		ctx.Logf("proxy: fail reject connect: %v", dialConfig.Fail)
		return goproxy.RejectConnect, host
	}

	// dial remote server before starting local handshake with client
	// if DANE handshake fails, reject the connect request early
	// a successful tls connection will be added to ctx to get consumed
	// by transport.
	dialConfig.Config = newDANEConfig(dialConfig.Host, dialConfig.TLSA)
	conn, err := dialTLSContext(context.Background(), dialConfig)
	if err != nil {
		ctx.Logf("proxy: dial tls failed: %v", dialConfig.Fail)
		return goproxy.RejectConnect, host
	}

	dialConfig.Conn = conn
	ctx.Logf("proxy: mitming connect")
	return goproxy.MitmConnect, host

}

func (c *Config) setupMITM(p *goproxy.ProxyHttpServer) error {
	if c.Certificate != nil && c.PrivateKey != nil {
		mc, err := newMITMConfig(c.Certificate, c.PrivateKey, c.Validity, "DNSSEC")
		if err != nil {
			return err
		}

		tlsConfig := func(host string, ctx *goproxy.ProxyCtx) (*tls.Config, error) {
			host, _, _ = net.SplitHostPort(host)
			return mc.tlsForHost(host, ctx), nil
		}

		goproxy.OkConnect = &goproxy.ConnectAction{Action: goproxy.ConnectAccept, TLSConfig: nil}
		goproxy.MitmConnect = &goproxy.ConnectAction{Action: goproxy.ConnectMitm, TLSConfig: tlsConfig}
		goproxy.HTTPMitmConnect = &goproxy.ConnectAction{Action: goproxy.ConnectHTTPMitm, TLSConfig: nil}
		goproxy.RejectConnect = &goproxy.ConnectAction{Action: goproxy.ConnectReject, TLSConfig: nil}

		p.OnRequest(tlsaFilterFunc(c)).HandleConnectFunc(handleConnect)
	}

	return nil
}

func (c *Config) Handler() (http.Handler, error) {
	p := goproxy.NewProxyHttpServer()
	// ConnectDial is only used for non mitm ed CONNECT requests
	// the configured resolver should still be used for all requests
	p.ConnectDial = dialFunc(c.Resolver)
	p.Verbose = c.Verbose
	if err := c.setupMITM(p); err != nil {
		return nil, err
	}

	p.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		ctx.RoundTripper = goproxy.RoundTripperFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (resp *http.Response, err error) {
			// the custom round tripper expects an tlsDialConfig in the ctx for DialTLSContext
			// it also uses the resolver for DialContext requests
			ctx.Logf("proxy: attempt round trip for %s", req.Host)
			tr := roundTripper(c.Resolver, ctx)
			resp, err = tr.RoundTrip(req)
			if err != nil {
				err = fmt.Errorf("proxy: unable to round trip %s: %v", req.Host, err)
			}
			return
		})
		return req, nil
	})

	return p, nil
}

func (c *Config) Run(addr string) error {
	h, err := c.Handler()
	if err != nil {
		return err
	}

	return http.ListenAndServe(addr, h)
}
