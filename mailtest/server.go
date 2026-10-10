// Package mailtest is an in-process SMTP server for tests of internal/mail
// and its users: plain, STARTTLS or implicit TLS, optional AUTH PLAIN, and
// scripted failures. It listens on 127.0.0.1 only.
package mailtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Modes.
const (
	Plain    = "plain"    // no TLS offered
	StartTLS = "starttls" // STARTTLS offered
	Implicit = "tls"      // TLS from the first byte
)

// Options configure a server.
type Options struct {
	Mode string
	// CertHost is the name in the certificate; default "localhost" (with the
	// IP 127.0.0.1). Another name makes verification fail.
	CertHost string
	// Username and Password, when set, are required (AUTH PLAIN).
	Username, Password string
	// FailData makes the first FailData DATA commands answer 451.
	FailData int
	// RejectRcpt makes RCPT TO answer 550.
	RejectRcpt bool
}

// Received is one accepted message.
type Received struct {
	From     string
	To       []string
	Data     string
	AuthUser string
	TLS      bool
}

// Server is a running test server.
type Server struct {
	Host string
	Port int
	// Roots trusts the server's certificate.
	Roots *x509.CertPool

	opts     Options
	tls      *tls.Config
	ln       net.Listener
	mu       sync.Mutex
	messages []Received
	failData int
	wg       sync.WaitGroup
}

// Start starts a server closed at the end of the test.
func Start(t testing.TB, opts Options) *Server {
	t.Helper()
	if opts.Mode == "" {
		opts.Mode = Plain
	}
	certHost := opts.CertHost
	if certHost == "" {
		certHost = "localhost"
	}
	cert, roots := certificate(t, certHost)
	s := &Server{Roots: roots, opts: opts, failData: opts.FailData,
		tls: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if opts.Mode == Implicit {
		ln = tls.NewListener(ln, s.tls)
	}
	s.ln = ln
	addr := ln.Addr().(*net.TCPAddr)
	s.Host, s.Port = "127.0.0.1", addr.Port
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() { ln.Close(); s.wg.Wait() })
	return s
}

// Messages returns the accepted messages so far.
func (s *Server) Messages() []Received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Received(nil), s.messages...)
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			s.session(conn)
		}()
	}
}

func (s *Server) session(conn net.Conn) {
	isTLS := s.opts.Mode == Implicit
	tp := textproto.NewConn(conn)
	reply := func(code int, lines ...string) {
		for i, l := range lines {
			sep := " "
			if i < len(lines)-1 {
				sep = "-"
			}
			tp.PrintfLine("%d%s%s", code, sep, l)
		}
	}
	reply(220, "mailtest ESMTP")
	var cur Received
	authUser := ""
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO":
			lines := []string{"mailtest"}
			if s.opts.Mode == StartTLS && !isTLS {
				lines = append(lines, "STARTTLS")
			}
			lines = append(lines, "AUTH PLAIN", "8BITMIME")
			reply(250, lines...)
		case "HELO":
			reply(250, "mailtest")
		case "STARTTLS":
			if s.opts.Mode != StartTLS || isTLS {
				reply(502, "not offered")
				continue
			}
			reply(220, "go ahead")
			tc := tls.Server(conn, s.tls)
			if err := tc.Handshake(); err != nil {
				return
			}
			conn, isTLS = tc, true
			tp = textproto.NewConn(tc)
		case "AUTH":
			mech, initial, _ := strings.Cut(arg, " ")
			raw, err := base64.StdEncoding.DecodeString(initial)
			parts := strings.Split(string(raw), "\x00")
			if !strings.EqualFold(mech, "PLAIN") || err != nil || len(parts) != 3 ||
				parts[1] != s.opts.Username || parts[2] != s.opts.Password {
				reply(535, "authentication failed")
				continue
			}
			authUser = parts[1]
			reply(235, "ok")
		case "MAIL":
			if s.opts.Username != "" && authUser == "" {
				reply(530, "authentication required")
				continue
			}
			cur = Received{From: addrArg(arg), AuthUser: authUser, TLS: isTLS}
			reply(250, "ok")
		case "RCPT":
			if s.opts.RejectRcpt {
				reply(550, "no such user")
				continue
			}
			cur.To = append(cur.To, addrArg(arg))
			reply(250, "ok")
		case "DATA":
			reply(354, "go ahead")
			data, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			s.mu.Lock()
			fail := s.failData > 0
			if fail {
				s.failData--
			} else {
				cur.Data = string(data)
				s.messages = append(s.messages, cur)
			}
			s.mu.Unlock()
			if fail {
				reply(451, "try again later")
				continue
			}
			reply(250, "queued")
		case "RSET", "NOOP":
			reply(250, "ok")
		case "QUIT":
			reply(221, "bye")
			return
		default:
			reply(502, "unknown command")
		}
	}
}

// addrArg takes the address out of "FROM:<a@b> ..." or "TO:<a@b>".
func addrArg(arg string) string {
	if i := strings.IndexByte(arg, '<'); i >= 0 {
		if j := strings.IndexByte(arg[i:], '>'); j > 0 {
			return arg[i+1 : i+j]
		}
	}
	return arg
}

// certificate makes a self-signed certificate for host (and 127.0.0.1 when
// host is localhost) and a pool that trusts it.
func certificate(t testing.TB, host string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true, BasicConstraintsValid: true,
		DNSNames: []string{host},
	}
	if host == "localhost" {
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// PortString returns the port as a string, for an {PREFIX}SMTP_PORT setting.
func (s *Server) PortString() string { return strconv.Itoa(s.Port) }
